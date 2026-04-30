//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"text/template"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
)

// ClusterSpec describes a Kind cluster with kube-vip.
type ClusterSpec struct {
	Name           string
	Nodes          int
	Networking     kindconfigv1alpha4.Networking
	KubeVip        KubevipManifestValues
	Logger         TestLogger
	ConfigMtx      *sync.Mutex
	KubeadmPatches []kindconfigv1alpha4.PatchJSON6902
	// TemplateName is the kube-vip manifest template filename relative to the
	// e2e test directory. Defaults to "kube-vip.yaml.tmpl".
	TemplateName string
}

// Cluster holds a running Kind cluster with kube-vip.
type Cluster struct {
	Name      string
	Client    kubernetes.Interface
	RestCfg   *rest.Config
	Nodes     []nodes.Node
	Provider  *kindcluster.Provider
	Logger    TestLogger
	ConfigMtx *sync.Mutex
}

// CreateCluster creates a Kind cluster, renders and mounts the kube-vip
// manifest, loads the kube-vip image, and returns the Cluster.
func CreateCluster(ctx context.Context, spec *ClusterSpec) *Cluster {
	c := &Cluster{
		Logger:    spec.Logger,
		ConfigMtx: spec.ConfigMtx,
	}

	// Fill defaults from env
	if spec.KubeVip.ImagePath == "" {
		spec.KubeVip.ImagePath = os.Getenv("E2E_IMAGE_PATH")
	}
	if spec.KubeVip.ConfigPath == "" {
		spec.KubeVip.ConfigPath = os.Getenv("CONFIG_PATH")
		if spec.KubeVip.ConfigPath == "" {
			spec.KubeVip.ConfigPath = "/etc/kubernetes/admin.conf"
		}
	}
	if spec.KubeVip.ControlPlaneEnable == "" {
		spec.KubeVip.ControlPlaneEnable = "true"
	}
	if spec.KubeVip.SvcEnable == "" {
		spec.KubeVip.SvcEnable = "false"
	}
	if spec.KubeVip.SvcElectionEnable == "" {
		spec.KubeVip.SvcElectionEnable = "false"
	}
	if spec.KubeVip.EnableNodeLabeling == "" {
		spec.KubeVip.EnableNodeLabeling = "false"
	}
	if spec.KubeVip.EnableEndpointslices == "" {
		spec.KubeVip.EnableEndpointslices = "false"
	}

	// Render kube-vip manifest
	curDir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	tmplName := spec.TemplateName
	if tmplName == "" {
		tmplName = "kube-vip.yaml.tmpl"
	}
	tmpl, err := template.New(tmplName).
		ParseFiles(filepath.Join(curDir, tmplName))
	Expect(err).NotTo(HaveOccurred())

	tmpDir, err := os.MkdirTemp("", "kube-vip-manifest")
	Expect(err).NotTo(HaveOccurred())
	manifestPath := filepath.Join(tmpDir, fmt.Sprintf("kube-vip-%s.yaml", spec.Name))
	manifestFile, err := os.Create(manifestPath)
	Expect(err).NotTo(HaveOccurred())
	Expect(tmpl.Execute(manifestFile, spec.KubeVip)).To(Succeed())
	manifestFile.Close()

	// Handle v1.29+ super-admin.conf for first node
	_, v129 := os.LookupEnv("V129")
	firstNodeManifestPath := manifestPath
	if v129 {
		firstNodeManifestPath = filepath.Join(tmpDir, fmt.Sprintf("kube-vip-%s-first.yaml", spec.Name))
		firstManifest, err := os.Create(firstNodeManifestPath)
		Expect(err).NotTo(HaveOccurred())
		firstNodeValues := spec.KubeVip
		firstNodeValues.ConfigPath = "/etc/kubernetes/super-admin.conf"
		Expect(tmpl.Execute(firstManifest, firstNodeValues)).To(Succeed())
		firstManifest.Close()
	}

	// Build Kind cluster config
	k8sImage := os.Getenv("K8S_IMAGE_PATH")
	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking:                    spec.Networking,
		KubeadmConfigPatchesJSON6902: spec.KubeadmPatches,
	}
	for i := range spec.Nodes {
		mPath := manifestPath
		if i == 0 && v129 {
			mPath = firstNodeManifestPath
		}
		node := kindconfigv1alpha4.Node{
			Role: kindconfigv1alpha4.ControlPlaneRole,
			ExtraMounts: []kindconfigv1alpha4.Mount{{
				HostPath:      mPath,
				ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
			}},
		}
		if k8sImage != "" {
			node.Image = k8sImage
		}
		clusterConfig.Nodes = append(clusterConfig.Nodes, node)
	}

	// Create cluster
	c.Provider = kindcluster.NewProvider(
		kindcluster.ProviderWithLogger(spec.Logger),
		kindcluster.ProviderWithDocker(),
	)
	format.UseStringerRepresentation = true
	c.Name = spec.Name
	Expect(c.Provider.Create(
		c.Name,
		kindcluster.CreateWithV1Alpha4Config(&clusterConfig),
		kindcluster.CreateWithRetain(os.Getenv("E2E_PRESERVE_CLUSTER") == "true"),
	)).To(Succeed())

	// Get kubeconfig and k8s client
	kc, err := c.Provider.KubeConfig(c.Name, false)
	Expect(err).ToNot(HaveOccurred())
	c.RestCfg, err = ClientConfigFromKubeconfig(kc)
	Expect(err).ToNot(HaveOccurred())
	c.Client, err = kubernetes.NewForConfig(c.RestCfg)
	Expect(err).ToNot(HaveOccurred())

	// Discover nodes
	c.Nodes, err = c.Provider.ListInternalNodes(c.Name)
	Expect(err).ToNot(HaveOccurred())
	Expect(len(c.Nodes)).To(BeNumerically(">=", spec.Nodes))

	// Load kube-vip image
	c.LoadImage(spec.KubeVip.ImagePath)

	return c
}

// LoadImage loads a Docker image into the Kind cluster.
func (c *Cluster) LoadImage(image string) {
	if err := LoadDockerImageToKind(c.Logger, image, c.Name); err != nil {
		By(fmt.Sprintf("failed to load image %s (will be pulled on deploy): %s", image, err))
	}
}

// Delete tears down the Kind cluster. Respects E2E_PRESERVE_CLUSTER.
func (c *Cluster) Delete() {
	if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
		return
	}
	By(fmt.Sprintf("deleting cluster: %s", c.Name))
	if c.ConfigMtx != nil {
		Eventually(func() error {
			c.ConfigMtx.Lock()
			defer c.ConfigMtx.Unlock()
			return c.Provider.Delete(c.Name, "")
		}, "60s", "200ms").Should(Succeed())
	} else {
		Expect(c.Provider.Delete(c.Name, "")).To(Succeed())
	}
}

// SaveLogs saves kube-vip pod logs to the given directory.
func (c *Cluster) SaveLogs(ctx context.Context, dir string) {
	By(fmt.Sprintf("saving logs to %q", dir))
	_ = GetLogs(ctx, c.Client, dir, c.Name)
}
