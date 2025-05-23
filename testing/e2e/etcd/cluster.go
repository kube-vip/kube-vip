//go:build e2e
// +build e2e

package etcd

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/gomega"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/slices"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"

	"github.com/kube-vip/kube-vip/testing/e2e"
)

type ClusterSpec struct {
	Nodes                int
	Name                 string
	VIP                  string
	KubeVIPImage         string
	KubeVIPpManifestPath string
	KubeletManifestPath  string
	KubeletFlagsPath     string
	EtcdCertsFolder      string
	Logger               e2e.TestLogger
}

type Cluster struct {
	*ClusterSpec
	Nodes []nodes.Node

	provider *cluster.Provider
}

func CreateCluster(ctx context.Context, spec *ClusterSpec) *Cluster {
	c := &Cluster{
		ClusterSpec: spec,
	}

	c.provider = cluster.NewProvider(
		cluster.ProviderWithLogger(spec.Logger),
		cluster.ProviderWithDocker(),
	)

	c.Logger.Printf("Creating kind nodes")
	c.initKindCluster()

	c.Logger.Printf("Loading kube-vip image into nodes")
	e2e.LoadDockerImageToKind(spec.Logger, spec.KubeVIPImage, spec.Name)

	c.Logger.Printf("Starting etcd cluster")
	c.initEtcd(ctx)

	c.Logger.Printf("Checking 1 node etcd is available through VIP")
	c.VerifyEtcdThroughVIP(ctx, 15*time.Second)

	c.Logger.Printf("Adding the rest of the nodes to the etcd cluster")
	c.joinRestOfNodes(ctx)

	c.Logger.Printf("Checking health for all nodes")
	for _, node := range c.Nodes {
		c.expectEtcdNodeHealthy(ctx, node, 15*time.Second)
	}

	c.Logger.Printf("Checking %d nodes etcd is available through VIP", c.ClusterSpec.Nodes)
	c.VerifyEtcdThroughVIP(ctx, 15*time.Second)

	return c
}

func (c *Cluster) initKindCluster() {
	kindCluster := &kindconfigv1alpha4.Cluster{
		Networking: kindconfigv1alpha4.Networking{
			IPFamily: kindconfigv1alpha4.IPv4Family,
		},
	}

	for i := 0; i < c.ClusterSpec.Nodes; i++ {
		kindCluster.Nodes = append(kindCluster.Nodes, kindconfigv1alpha4.Node{
			Role: kindconfigv1alpha4.ControlPlaneRole,
			ExtraMounts: []kindconfigv1alpha4.Mount{
				{
					HostPath:      c.ClusterSpec.KubeVIPpManifestPath,
					ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
				},
				{
					HostPath:      c.ClusterSpec.KubeletManifestPath,
					ContainerPath: "/var/lib/kubelet/config.yaml",
				},
				{
					HostPath:      c.ClusterSpec.KubeletFlagsPath,
					ContainerPath: "/etc/default/kubelet",
				},
			},
		})
	}

	Expect(c.provider.Create(
		c.Name,
		cluster.CreateWithV1Alpha4Config(kindCluster),
		cluster.CreateWithRetain(true),
		cluster.CreateWithStopBeforeSettingUpKubernetes(true),
		cluster.CreateWithWaitForReady(2*time.Minute),
		cluster.CreateWithNodeImage("public.ecr.aws/eks-anywhere/kubernetes-sigs/kind/node:v1.26.7-eks-d-1-26-16-eks-a-47"),
	)).To(Succeed())
}

func (c *Cluster) initEtcd(ctx context.Context) {
	var err error
	c.Nodes, err = c.provider.ListInternalNodes(c.Name)
	slices.SortFunc(c.Nodes, func(a, b nodes.Node) int {
		aName := a.String()
		bName := b.String()
		if aName < bName {
			return 1
		} else if aName > bName {
			return -1
		}

		return 0
	})

	Expect(err).NotTo(HaveOccurred())
	firstNode := c.Nodes[0]

	createCerts(firstNode)

	// We need to run all phases individually to be able to re-run the health phase
	// In CI it can take longer than the 30 seconds timeout that is hardcoded in etcdadm
	// If etcdadm added the option to configure this timeout, we could change this to just
	// call etcdadm init to run all phases.

	flags := []string{
		"--init-system", "kubelet",
		"--certs-dir", "/etc/kubernetes/pki/etcd",
		"--server-cert-extra-sans", strings.Join([]string{"etcd", c.VIP, e2e.NodeIPv4(firstNode)}, ","),
		"--version", "3.5.8-eks-1-26-16",
		"--image-repository", "public.ecr.aws/eks-distro/etcd-io/etcd",
	}

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "install", flags)...,
	)

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "certificates", flags)...,
	)

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "snapshot", flags)...,
	)

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "configure", flags)...,
	)

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "start", flags)...,
	)

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "etcdctl", flags)...,
	)

	Eventually(func() error {
		return runInNode(firstNode,
			"etcdadm",
			initArgsForPhase("init", "health", flags)...,
		)
	}, 3*time.Minute).Should(Succeed(), "etcd should become healthy in node in less than 3 minutes")

	e2e.RunInNode(firstNode,
		"etcdadm",
		initArgsForPhase("init", "post-init-instructions", flags)...,
	)

	bindEtcdListenerToAllIPs(firstNode)

	e2e.CopyFolderFromNodeToDisk(firstNode, "/etc/kubernetes/pki/etcd", c.EtcdCertsFolder)

	c.expectEtcdNodeHealthy(ctx, firstNode, 15*time.Second)
}

func runInNode(node nodes.Node, command string, args ...string) error {
	return e2e.PrintCommandOutputIfErr(node.Command(command, args...).Run())
}

func initArgsForPhase(command, phaseName string, flags []string) []string {
	c := make([]string, 0, 3+len(flags))
	c = append(c, command, "phase", phaseName)
	c = append(c, flags...)
	return c
}

func (c *Cluster) joinRestOfNodes(ctx context.Context) {
	for _, node := range c.Nodes[1:] {
		c.joinNode(ctx, c.Nodes[0], node)
	}
}

func (c *Cluster) joinNode(ctx context.Context, firstNode, node nodes.Node) {
	nodeutils.CopyNodeToNode(firstNode, node, "/etc/kubernetes/pki/ca.crt")
	nodeutils.CopyNodeToNode(firstNode, node, "/etc/kubernetes/pki/ca.key")
	nodeutils.CopyNodeToNode(firstNode, node, "/etc/kubernetes/pki/etcd/ca.crt")
	nodeutils.CopyNodeToNode(firstNode, node, "/etc/kubernetes/pki/etcd/ca.key")

	e2e.RunInNode(node,
		"etcdadm",
		"join",
		"https://"+e2e.NodeIPv4(firstNode)+":2379",
		"--init-system", "kubelet",
		"--certs-dir", "/etc/kubernetes/pki/etcd",
		"--server-cert-extra-sans", strings.Join([]string{"etcd", c.VIP, e2e.NodeIPv4(node)}, ","),
		"--version", "3.5.8-eks-1-26-16",
		"--image-repository", "public.ecr.aws/eks-distro/etcd-io/etcd",
	)

	bindEtcdListenerToAllIPs(node)

	c.expectEtcdNodeHealthy(ctx, node, 30*time.Second)
}

func (c *Cluster) DeleteEtcdMember(ctx context.Context, toDelete, toKeep nodes.Node) {
	// point client to the node we are keeping because we are going to use it to remove the other node
	// and vip is possibly pointing to that node
	client := c.newEtcdClient(e2e.NodeIPv4(toDelete))
	defer client.Close()
	members, err := client.MemberList(ctx)
	Expect(err).NotTo(HaveOccurred())
	c.Logger.Printf("Members: %v", members.Members)

	nodeName := toDelete.String()
	for _, m := range members.Members {
		if m.Name == nodeName {
			c.Logger.Printf("Removing node %s with memberID %d", m.Name, m.ID)
			// We need to retry this request because etcd will reject it if the
			// server doesn't have recent connections to enough active members
			// to protect the quorum. (active - 1) >= 1+((members-1)/2)
			Eventually(func() error {
				_, err := client.MemberRemove(ctx, m.ID)
				return err
			}).WithPolling(time.Second).WithTimeout(10*time.Second).Should(
				Succeed(), "removing member should succeed once all members have connections to each other",
			)

			break
		}
	}

	e2e.DeleteNodes(toDelete)
}

func (c *Cluster) Delete() {
	Expect(c.provider.Delete(c.Name, "")).To(Succeed())
}

func createCerts(node nodes.Node) {
	e2e.RunInNode(node,
		"kubeadm",
		"init",
		"phase", "certs", "ca", "--config", "/kind/kubeadm.conf",
	)

	e2e.RunInNode(node,
		"kubeadm",
		"init",
		"phase", "certs", "etcd-ca", "--config", "/kind/kubeadm.conf",
	)
}

func bindEtcdListenerToAllIPs(node nodes.Node) {
	// There is no easy way to make etcdadm configure etcd to bind to 0.0.0.0
	// so we just manually update the manifest after it's created and restart it
	// We want to listen in 0.0.0.0 so our kube-vip can connect to it.
	e2e.RunInNode(node,
		"sed", "-i", `s/https:\/\/.*,https:\/\/127.0.0.1:2379/https:\/\/0.0.0.0:2379/g`, "/etc/kubernetes/manifests/etcd.manifest",
	)

	e2e.StopPodInNode(node, "etcd")

	e2e.RunInNode(node,
		"systemctl", "restart", "kubelet",
	)
}

func (c *Cluster) newEtcdClient(serverIPs ...string) *clientv3.Client {
	tlsInfo := transport.TLSInfo{
		TrustedCAFile: filepath.Join(c.EtcdCertsFolder, "ca.crt"),
		CertFile:      filepath.Join(c.EtcdCertsFolder, "etcdctl-etcd-client.crt"),
		KeyFile:       filepath.Join(c.EtcdCertsFolder, "etcdctl-etcd-client.key"),
	}

	clientTLS, err := tlsInfo.ClientConfig()
	Expect(err).NotTo(HaveOccurred())

	endpoints := make([]string, 0, len(serverIPs))
	for _, ip := range serverIPs {
		endpoints = append(endpoints, ip+":2379")
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		TLS:         clientTLS,
		DialTimeout: time.Second,
	})
	Expect(err).NotTo(HaveOccurred())

	return client
}

func (c *Cluster) VerifyEtcdThroughVIP(ctx context.Context, timeout time.Duration) {
	etcdClient := c.newEtcdClient(c.VIP)
	defer etcdClient.Close()
	rCtx, cancel := context.WithTimeout(ctx, timeout)
	_, err := etcdClient.MemberList(rCtx)
	Expect(err).NotTo(HaveOccurred())
	cancel()
}
