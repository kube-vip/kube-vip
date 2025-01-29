package deployment

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gookit/slog"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cmd"
	load "sigs.k8s.io/kind/pkg/cmd/kind/load/docker-image"
)

var provider *cluster.Provider

type kubevipManifestValues struct {
	ControlPlaneVIP string
	ImagePath       string
}

type nodeAddresses struct {
	node      string
	addresses []string
}

func (config *TestConfig) CreateKind() error {

	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking: kindconfigv1alpha4.Networking{
			IPFamily: kindconfigv1alpha4.IPv4Family,
		},
		Nodes: []kindconfigv1alpha4.Node{
			{
				Role: kindconfigv1alpha4.ControlPlaneRole,
			},
		},
	}

	if config.IPv6 {
		// Change Networking Family
		clusterConfig.Networking.IPFamily = kindconfigv1alpha4.IPv6Family
	}
	if config.DualStack {
		// Change Networking Family
		clusterConfig.Networking.IPFamily = kindconfigv1alpha4.DualStackFamily
	}

	if config.Cilium {
		clusterConfig.Networking.DisableDefaultCNI = true
	}

	if config.ControlPlane {
		err := config.manifestGen()
		if err != nil {
			return err
		}

		// Add two additional control plane nodes (3)
		clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{Role: kindconfigv1alpha4.ControlPlaneRole})
		clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{Role: kindconfigv1alpha4.ControlPlaneRole})

		// Add the extra static pod manifest
		mount := kindconfigv1alpha4.Mount{
			HostPath:      config.ManifestPath,
			ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
		}
		for x := range clusterConfig.Nodes {
			if clusterConfig.Nodes[x].Role == kindconfigv1alpha4.ControlPlaneRole {
				clusterConfig.Nodes[x].ExtraMounts = append(clusterConfig.Nodes[x].ExtraMounts, mount)
			}
		}
	} else {
		// Add three additional worker nodes
		clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{Role: kindconfigv1alpha4.WorkerRole})
		clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{Role: kindconfigv1alpha4.WorkerRole})
		//clusterConfig.Nodes = append(clusterConfig.Nodes, kindconfigv1alpha4.Node{Role: kindconfigv1alpha4.WorkerRole})
	}

	// Change the default image if required
	if config.KindVersionImage != "" {
		for x := range clusterConfig.Nodes {
			clusterConfig.Nodes[x].Image = config.KindVersionImage
		}
	}

	provider = cluster.NewProvider(cluster.ProviderWithLogger(cmd.NewLogger()), cluster.ProviderWithDocker())
	clusters, err := provider.List()
	if err != nil {
		return err
	}
	found := false
	for x := range clusters {
		if clusters[x] == "services" {
			slog.Infof("Cluster already exists")
			found = true
		}
	}
	if !found {
		err := provider.Create("services", cluster.CreateWithV1Alpha4Config(&clusterConfig))
		if err != nil {
			return err
		}

		loadImageCmd := load.NewCommand(cmd.NewLogger(), cmd.StandardIOStreams())
		loadImageCmd.SetArgs([]string{"--name", "services", config.ImagePath})
		err = loadImageCmd.Execute()
		if err != nil {
			return err
		}
		nodes, err := provider.ListNodes("services")
		if err != nil {
			return err
		}

		if !config.SkipHostnameChange {
			slog.Infof("⚙️ changing hostnames on nodes to force using proper node names for service selection")
			for _, node := range nodes {
				nodeName := node.String()
				cmd := exec.Command("docker", "exec", nodeName, "hostname", nodeName+"-modified")
				if _, err := cmd.CombinedOutput(); err != nil {
					return err
				}
			}
		}

		if config.Cilium {
			cmd := exec.Command("cilium", "install", "--helm-set", "ipv6.enabled=true", "--helm-set", "--enable-ipv6-ndp=true")
			_, _ = cmd.CombinedOutput()
		}

		// HMMM, if we want to run workloads on the control planes (todo)
		if config.ControlPlane {
			for _, node := range nodes {
				nodeName := node.String()
				cmd := exec.Command("kubectl", "taint", "nodes", nodeName, "node-role.kubernetes.io/control-plane:NoSchedule-")
				_, _ = cmd.CombinedOutput()
			}
		}

		globalRange := "172.18.100.10-172.18.100.30"
		if config.IPv6 {
			globalRange = "fd34:70db:8529:1e3d:0000:0000:0000:0010-fd34:70db:8529:1e3d:0000:0000:0000:0030"
		}

		cmd := exec.Command("kubectl", "create", "configmap", "--namespace", "kube-system", "kubevip", "--from-literal", "range-global="+globalRange)
		if _, err := cmd.CombinedOutput(); err != nil {
			return err
		}
		cmd = exec.Command("kubectl", "create", "-f", "https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml")
		if _, err := cmd.CombinedOutput(); err != nil {
			return err
		}
		cmd = exec.Command("kubectl", "create", "-f", "https://kube-vip.io/manifests/rbac.yaml")
		if _, err := cmd.CombinedOutput(); err != nil {
			return err
		}
		slog.Infof("💤 sleeping for a few seconds to let controllers start")
		time.Sleep(time.Second * 5)
	}
	return nil
}

func DeleteKind() error {
	slog.Info("🧽 deleting Kind cluster")
	return provider.Delete("services", "")
}

func getAddressesOnNodes() ([]nodeAddresses, error) {
	nodesConfig := []nodeAddresses{}
	nodes, err := provider.ListNodes("services")
	if err != nil {
		return nodesConfig, err
	}
	for x := range nodes {
		var b bytes.Buffer

		exec := nodes[x].Command("hostname", "--all-ip-addresses")
		exec.SetStderr(&b)
		exec.SetStdin(&b)
		exec.SetStdout(&b)
		err = exec.Run()
		if err != nil {
			return nodesConfig, err
		}
		nodesConfig = append(nodesConfig, nodeAddresses{
			node:      nodes[x].String(),
			addresses: strings.Split(b.String(), " "),
		})
	}
	return nodesConfig, nil
}

func checkNodesForDuplicateAddresses(nodes []nodeAddresses, address string) error {
	var foundOnNode []string
	// Iterate over all nodes to find addresses, where there is an address match add to array
	for x := range nodes {
		for y := range nodes[x].addresses {
			if nodes[x].addresses[y] == address {
				foundOnNode = append(foundOnNode, nodes[x].node)
			}
		}
	}
	// If one address is on multiple nodes, then something has gone wrong
	if len(foundOnNode) > 1 {
		return fmt.Errorf("‼️ multiple nodes [%s] have address [%s]", strings.Join(foundOnNode, " "), address)
	}
	return nil
}
