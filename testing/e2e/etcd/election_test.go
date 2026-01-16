//go:build e2e
// +build e2e

package etcd_test

import (
	"context"
	"os"
	"path/filepath"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"k8s.io/klog/v2"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/e2e/etcd"
)

type testConfig struct {
	logger              e2e.TestLogger
	kubeVipImage        string
	kubeVipManifestPath string
	clusterName         string
	vip                 string
	etcdCertsFolder     string
	currentDir          string
	cluster             *etcd.Cluster
}

func (t *testConfig) cleanup() {
	if os.Getenv("E2E_PRESERVE_CLUSTER") == "true" {
		return
	}

	t.cluster.Delete()
	Expect(os.RemoveAll(t.kubeVipManifestPath)).To(Succeed())
	Expect(os.RemoveAll(t.etcdCertsFolder)).To(Succeed())
}

var _ = Describe("kube-vip with etcd leader election", func() {
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	test := &testConfig{}

	AfterEach(func() {
		test.cleanup()
	})

	BeforeEach(func() {
		By("configuring test", func() {
			var err error
			format.UseStringerRepresentation = true // Otherwise error stacks have binary format.
			klog.SetOutput(GinkgoWriter)

			test.clusterName = "kube-vip-etcd-test" // this needs to unique per it block
			test.logger = e2e.TestLogger{}
			test.etcdCertsFolder = "certs"

			test.kubeVipImage = os.Getenv("E2E_IMAGE_PATH")

			test.vip = e2e.GenerateVIP(utils.IPv4Family, 5)
			test.logger.Printf("Selected VIP %s", test.vip)

			test.currentDir, err = os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			tempDirPath, err := os.MkdirTemp("", "kube-vip-test")
			Expect(err).NotTo(HaveOccurred())

			test.kubeVipManifestPath = filepath.Join(tempDirPath, "etcd-vip-ipv4.yaml")
			manifestFile, err := os.Create(test.kubeVipManifestPath)
			Expect(err).NotTo(HaveOccurred())
			defer manifestFile.Close()

			templatePath := filepath.Join(test.currentDir, "kube-etcd-vip.yaml.tmpl")
			kubeVIPManifestTemplate, err := template.New("kube-etcd-vip.yaml.tmpl").ParseFiles(templatePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(kubeVIPManifestTemplate.Execute(manifestFile, e2e.KubevipManifestValues{
				ControlPlaneVIP: test.vip,
				ImagePath:       test.kubeVipImage,
			})).To(Succeed())
		})

		By("creating etcd cluster", func() {
			spec := &etcd.ClusterSpec{
				Name:                 test.clusterName,
				Nodes:                2,
				VIP:                  test.vip,
				KubeVIPImage:         test.kubeVipImage,
				KubeVIPpManifestPath: test.kubeVipManifestPath,
				KubeletManifestPath:  filepath.Join(test.currentDir, "kubelet.yaml"),
				KubeletFlagsPath:     filepath.Join(test.currentDir, "kubelet-flags.env"),
				EtcdCertsFolder:      filepath.Join(test.currentDir, test.etcdCertsFolder),
				Logger:               test.logger,
			}

			test.cluster = etcd.CreateCluster(ctx, spec)
		})
	})

	When("an etcd node is removed", func() {
		It("elects a new kube-vip leader and provides a VIP to the second node", func() {
			By("removing as member and killing the first node", func() {
				test.cluster.DeleteEtcdMember(ctx, test.cluster.Nodes[0], test.cluster.Nodes[1])
			})
			By("verifying etcd is up and accessible through the vip", func() {
				test.cluster.VerifyEtcdThroughVIP(ctx, 40*time.Second)
			})
		})
	})
})
