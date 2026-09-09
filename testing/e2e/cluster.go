//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/yaml"
)

// ClusterSpec describes a Kind cluster with kube-vip.
type ClusterSpec struct {
	Name           string
	Nodes          int // control-plane node count
	WorkerNodes    int
	Networking     kindconfigv1alpha4.Networking
	KubeVip        KubevipManifestValues
	Logger         TestLogger
	ConfigMtx      *sync.Mutex
	KubeadmPatches []kindconfigv1alpha4.PatchJSON6902
	// TemplateName is the kube-vip manifest template filename relative to the
	// e2e test directory. Defaults to "kube-vip.yaml.tmpl".
	TemplateName string
	// UseDaemonSet deploys the rendered kube-vip pod spec as a daemonset rather
	// than mounting it as a static pod in each Kind node.
	UseDaemonSet bool
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

// CreateCluster creates a Kind cluster, renders and deploys the kube-vip
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
	if spec.KubeVip.EnableEndpoints == "" {
		spec.KubeVip.EnableEndpoints = "true"
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
	Expect(renderKubeVipManifest(tmpl, manifestPath, spec.KubeVip)).To(Succeed())
	workerManifestPath := manifestPath
	if spec.WorkerNodes > 0 {
		workerManifestPath = filepath.Join(tmpDir, fmt.Sprintf("kube-vip-%s-worker.yaml", spec.Name))
		workerValues := spec.KubeVip
		workerValues.ConfigPath = "/etc/kubernetes/kubelet.conf"
		workerValues.KubeletPKIPath = "/var/lib/kubelet/pki"
		Expect(renderKubeVipManifest(tmpl, workerManifestPath, workerValues)).To(Succeed())
	}

	// Handle v1.29+ super-admin.conf for first node
	_, v129 := os.LookupEnv("V129")
	firstNodeManifestPath := manifestPath
	if v129 {
		firstNodeManifestPath = filepath.Join(tmpDir, fmt.Sprintf("kube-vip-%s-first.yaml", spec.Name))
		firstNodeValues := spec.KubeVip
		firstNodeValues.ConfigPath = "/etc/kubernetes/super-admin.conf"
		Expect(renderKubeVipManifest(tmpl, firstNodeManifestPath, firstNodeValues)).To(Succeed())
	}

	// Build Kind cluster config
	k8sImage := os.Getenv("K8S_IMAGE_PATH")
	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking:                   spec.Networking,
		KubeadmConfigPatchesJSON6902: spec.KubeadmPatches,
	}
	appendNode := func(role kindconfigv1alpha4.NodeRole, manifest string) {
		node := kindconfigv1alpha4.Node{
			Role: role,
		}
		if !spec.UseDaemonSet {
			node.ExtraMounts = []kindconfigv1alpha4.Mount{{
				HostPath:      manifest,
				ContainerPath: "/etc/kubernetes/manifests/kube-vip.yaml",
			}}
		}
		if k8sImage != "" {
			node.Image = k8sImage
		}
		clusterConfig.Nodes = append(clusterConfig.Nodes, node)
	}
	for i := 0; i < spec.Nodes; i++ {
		mPath := manifestPath
		if i == 0 && v129 {
			mPath = firstNodeManifestPath
		}
		appendNode(kindconfigv1alpha4.ControlPlaneRole, mPath)
	}
	for i := 0; i < spec.WorkerNodes; i++ {
		appendNode(kindconfigv1alpha4.WorkerRole, workerManifestPath)
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
	Expect(len(c.Nodes)).To(BeNumerically(">=", spec.Nodes+spec.WorkerNodes))

	// Load kube-vip image
	c.LoadImage(spec.KubeVip.ImagePath)
	if spec.UseDaemonSet {
		deployDaemonSet(ctx, c, manifestPath)
	}
	Eventually(func() error {
		return kubeVipPodsReady(ctx, c.Client, spec.Nodes+spec.WorkerNodes)
	}, "120s", "2s").Should(Succeed())

	return c
}

func kubeVipPodsReady(ctx context.Context, client kubernetes.Interface, expected int) error {
	pods, err := client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "app=kube-vip"})
	if err != nil {
		return err
	}
	ready := 0
	states := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		isReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}
		if isReady {
			ready++
		}
		states = append(states, fmt.Sprintf("%s=%s ready=%t", pod.Name, pod.Status.Phase, isReady))
	}
	if ready < expected {
		return fmt.Errorf("kube-vip pods are not ready: %d/%d; states: %v", ready, expected, states)
	}
	return nil
}

func renderKubeVipManifest(tmpl *template.Template, path string, values KubevipManifestValues) (err error) {
	manifest, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create kube-vip manifest %q: %w", path, err)
	}
	defer func() {
		if closeErr := manifest.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close kube-vip manifest %q: %w", path, closeErr)
		}
	}()
	if err := tmpl.Execute(manifest, values); err != nil {
		return fmt.Errorf("render kube-vip manifest %q: %w", path, err)
	}
	return nil
}

func deployDaemonSet(ctx context.Context, c *Cluster, manifestPath string) {
	payload, err := os.ReadFile(manifestPath)
	Expect(err).NotTo(HaveOccurred())

	var pod corev1.Pod
	Expect(yaml.Unmarshal(payload, &pod)).To(Succeed())
	labels := map[string]string{"app": "kube-vip"}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	pod.Spec.Tolerations = append(pod.Spec.Tolerations,
		corev1.Toleration{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
		corev1.Toleration{
			Key:      "node-role.kubernetes.io/master",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
	)

	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-vip",
			Namespace: "kube-system",
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       pod.Spec,
			},
		},
	}

	_, err = c.Client.AppsV1().DaemonSets("kube-system").Create(ctx, daemonSet, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() error {
		current, getErr := c.Client.AppsV1().DaemonSets("kube-system").Get(ctx, daemonSet.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if current.Status.NumberReady != current.Status.DesiredNumberScheduled {
			return fmt.Errorf("kube-vip daemonset is not ready: %d/%d", current.Status.NumberReady, current.Status.DesiredNumberScheduled)
		}
		return nil
	}, "120s", "2s").Should(Succeed())
}

// WaitForKubeVipReady verifies that every requested node has a running and
// ready kube-vip static pod and container before callers access its endpoints.
func WaitForKubeVipReady(ctx context.Context, client kubernetes.Interface, nodeNames []string) error {
	pods, err := client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "app=kube-vip"})
	if err != nil {
		return fmt.Errorf("list kube-vip static pods: %w", err)
	}

	wanted := make(map[string]struct{}, len(nodeNames))
	for _, nodeName := range nodeNames {
		wanted[nodeName] = struct{}{}
	}
	ready := make(map[string]struct{}, len(nodeNames))
	status := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if _, ok := wanted[pod.Spec.NodeName]; !ok {
			continue
		}
		containerReady := false
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name == "kube-vip" && container.Ready && container.State.Running != nil {
				containerReady = true
				break
			}
		}
		podReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				podReady = true
				break
			}
		}
		status = append(status, fmt.Sprintf("%s=%s/podReady:%t/containerReady:%t", pod.Spec.NodeName, pod.Status.Phase, podReady, containerReady))
		if pod.Status.Phase == corev1.PodRunning && podReady && containerReady {
			ready[pod.Spec.NodeName] = struct{}{}
		}
	}

	missing := make([]string, 0)
	for nodeName := range wanted {
		if _, ok := ready[nodeName]; !ok {
			missing = append(missing, nodeName)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		sort.Strings(status)
		return fmt.Errorf("kube-vip static pods not ready on nodes %v (observed: %s)", missing, strings.Join(status, ", "))
	}
	return nil
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
	_ = c.saveKubeVipNodeDiagnostics(ctx, dir)
}

func (c *Cluster) saveKubeVipNodeDiagnostics(ctx context.Context, dir string) error {
	if os.Getenv("E2E_KEEP_LOGS") != "true" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create diagnostics directory: %w", err)
	}

	const script = `set +e
echo '--- static pod manifest'
cat /etc/kubernetes/manifests/kube-vip.yaml
	echo '--- kubelet kubeconfig references'
	grep -E 'server:|client-certificate:|client-key:' /etc/kubernetes/kubelet.conf
	echo '--- kubelet client credentials'
	ls -la /var/lib/kubelet/pki
echo '--- kube-vip pod sandboxes'
crictl pods --name kube-vip
echo '--- kube-vip containers'
crictl ps -a --name kube-vip
echo '--- listening sockets'
ss -lntp
echo '--- recent kubelet log'
journalctl -u kubelet --no-pager -n 200`
	for _, node := range c.Nodes {
		cmd := exec.CommandContext(ctx, "docker", "exec", node.String(), "sh", "-c", script) //nolint:gosec
		output, err := cmd.CombinedOutput()
		if err != nil {
			output = append(output, []byte(fmt.Sprintf("\ndiagnostic command failed: %v\n", err))...)
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-diagnostics.log", node.String()))
		if err := os.WriteFile(path, output, 0o600); err != nil {
			return fmt.Errorf("write node diagnostics %q: %w", path, err)
		}
	}
	return nil
}
