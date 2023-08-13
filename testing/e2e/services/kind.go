package main

import (
	"os/exec"
	"time"

	log "github.com/sirupsen/logrus"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cmd"
	load "sigs.k8s.io/kind/pkg/cmd/kind/load/docker-image"
)

var provider *cluster.Provider

func createKind(imagePath string) error {
	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking: kindconfigv1alpha4.Networking{
			IPFamily: kindconfigv1alpha4.IPv4Family,
		},
		Nodes: []kindconfigv1alpha4.Node{
			{
				Role: kindconfigv1alpha4.ControlPlaneRole,
			},
			{
				Role: kindconfigv1alpha4.WorkerRole,
			},
			{
				Role: kindconfigv1alpha4.WorkerRole,
			},
			{
				Role: kindconfigv1alpha4.WorkerRole,
			},
		},
	}

	provider = cluster.NewProvider(cluster.ProviderWithLogger(cmd.NewLogger()), cluster.ProviderWithDocker())
	clusters, err := provider.List()
	if err != nil {
		return err
	}
	found := false
	for x := range clusters {
		if clusters[x] == "services" {
			log.Infof("Cluster already exists")
			found = true
		}
	}
	if !found {
		err := provider.Create("services", cluster.CreateWithV1Alpha4Config(&clusterConfig))
		if err != nil {
			return err

		}
		loadImageCmd := load.NewCommand(cmd.NewLogger(), cmd.StandardIOStreams())
		loadImageCmd.SetArgs([]string{"--name", "services", imagePath})
		err = loadImageCmd.Execute()
		if err != nil {
			return err
		}
		cmd := exec.Command("kubectl", "create", "configmap", "--namespace", "kube-system", "kubevip", "--from-literal", "range-global=172.18.100.10-172.18.100.30")
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
		log.Infof("💤 sleeping for a few seconds to let controllers start")
		time.Sleep(time.Second * 5)
	}
	return nil
}

func deleteKind() error {
	log.Info("🧽 deleting Kind cluster")
	return provider.Delete("services", "")
}
