//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/services/pkg/deployment"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	api "github.com/osrg/gobgp/v3/api"
)

const (
	goBGPAS   uint32 = 65500
	kubevipAS uint32 = 65501
	goBGPPort uint32 = 50051

	defaultFixedNexthopv6 = "fc00:1000:1000:1000::100"
	defaultFixedNexthopv4 = "172.18.0.100"
)

var _ = Describe("kube-vip BGP mode", Ordered, func() {
	if Mode == ModeBGP {
		var (
			logger                     log.Logger
			imagePath                  string
			k8sImagePath               string
			configPath                 string
			kubeVIPBGPManifestTemplate *template.Template
			goBGPConfigTemplate        *template.Template
			tempDirPath                string
			v129                       bool
			localIPv4                  string
			localIPv6                  string
			curDir                     string
			networkInterface           string

			bgpKill chan any
		)

		BeforeAll(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}

			imagePath = os.Getenv("E2E_IMAGE_PATH")    // Path to kube-vip image
			configPath = os.Getenv("CONFIG_PATH")      // path to the api server config
			k8sImagePath = os.Getenv("K8S_IMAGE_PATH") // path to the kubernetes image (version for kind)
			if configPath == "" {
				configPath = "/etc/kubernetes/admin.conf"
			}
			if networkInterface = os.Getenv("NETWORK_INTERFACE"); networkInterface == "" {
				networkInterface = "br-"
			}

			_, v129 = os.LookupEnv("V129")
			var err error
			curDir, err = os.Getwd()
			Expect(err).NotTo(HaveOccurred())

			templateBGPPath := filepath.Join(curDir, "kube-vip-bgp.yaml.tmpl")
			kubeVIPBGPManifestTemplate, err = template.New("kube-vip-bgp.yaml.tmpl").ParseFiles(templateBGPPath)
			Expect(err).NotTo(HaveOccurred())

			tempDirPath, err = os.MkdirTemp("", "kube-vip-test")
			Expect(err).NotTo(HaveOccurred())
			localIPv4, err = deployment.GetLocalIPv4(networkInterface)
			Expect(err).ToNot(HaveOccurred())
			localIPv6, err = deployment.GetLocalIPv6(networkInterface)
			Expect(err).ToNot(HaveOccurred())

			goBGPConfig := &e2e.BGPPeerValues{
				AS: goBGPAS,
			}

			bgpKill = make(chan any)

			goBGPConfigPath := filepath.Join(filepath.Join(curDir, "bgp"), "config.toml.tmpl")
			goBGPConfigTemplate, err = template.New("config.toml.tmpl").ParseFiles(goBGPConfigPath)
			Expect(err).ToNot(HaveOccurred())

			goBGPConfigPath = filepath.Join(tempDirPath, "config.toml")

			f, err := os.OpenFile(goBGPConfigPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			Expect(err).ToNot(HaveOccurred())
			defer f.Close()

			err = goBGPConfigTemplate.Execute(f, goBGPConfig)
			Expect(err).ToNot(HaveOccurred())

			go startGoBGP(goBGPConfigPath, bgpKill)
		})

		AfterAll(func() {
			close(bgpKill)
		})

		Describe("kube-vip IPv4 services BGP mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues

				nodesNumber = 1
			)

			BeforeAll(func() {
				setupEnv(&tempDirPath, &cpVIP, &clusterName, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, e2e.IPv4Family, e2e.IPv4Family, []string{e2e.IPv4Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-ipv4")
			})

			AfterAll(func() {
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(context.TODO(), &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName, trafficPolicy, client, 1, gobgpClient, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName, trafficPolicy, client, 2, gobgpClient, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip IPv6 services BGP mode functionality", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues

				nodesNumber = 1
			)

			BeforeAll(func() {
				setupEnv(&tempDirPath, &cpVIP, &clusterName, manifestValues, localIPv4, localIPv6, imagePath, configPath,
					k8sImagePath, e2e.IPv6Family, e2e.IPv6Family, []string{e2e.IPv6Family}, &client, &gobgpPeers, v129,
					kubeVIPBGPManifestTemplate, &gobgpClient, logger, nodesNumber, "", "bgp-ipv4")
			})

			AfterAll(func() {
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(context.TODO(), &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes for services",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName, trafficPolicy, client, 1, gobgpClient, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName, trafficPolicy, client, 2, gobgpClient, "")
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - fixed nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues

				nodesNumber = 1
			)

			BeforeAll(func() {
				setupEnv(&tempDirPath, &cpVIP, &clusterName, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, e2e.IPv4Family, []string{e2e.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "mpbgp-ipv4")
			})

			AfterAll(func() {
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(context.TODO(), &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName, trafficPolicy, client, 1, gobgpClient, defaultFixedNexthopv6)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName, trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv6)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - fixed nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues

				nodesNumber = 1
			)

			BeforeAll(func() {
				setupEnv(&tempDirPath, &cpVIP, &clusterName, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, e2e.IPv6Family, []string{e2e.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "fixed", "mpbgp-ipv6")
			})

			AfterAll(func() {
				for _, n := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(context.TODO(), &api.DeletePeerRequest{
						Address: n.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName, trafficPolicy, client, 1, gobgpClient, defaultFixedNexthopv4)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName, trafficPolicy, client, 2, gobgpClient, defaultFixedNexthopv4)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv6 over IPv4 - auto_sourceif nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				containerIP    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				_, containerIP = setupEnv(&tempDirPath, &cpVIP, &clusterName, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamily, e2e.IPv4Family, []string{e2e.IPv4Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv4")
			})

			AfterAll(func() {
				for _, p := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(context.TODO(), &api.DeletePeerRequest{
						Address: p.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv6 routes over IPv4 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName, trafficPolicy, client, 1, gobgpClient, containerIP)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv6Family, api.Family_AFI_IP6, []corev1.IPFamily{corev1.IPv6Protocol}, svcName, trafficPolicy, client, 2, gobgpClient, containerIP)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})

		Describe("kube-vip DualStack services BGP mode functionality with MP-BGP IPv4 over IPv6 - fixed nexthop", Ordered, func() {
			var (
				cpVIP          string
				clusterName    string
				client         kubernetes.Interface
				manifestValues *e2e.KubevipManifestValues
				gobgpClient    api.GobgpApiClient
				gobgpPeers     []*e2e.BGPPeerValues
				containerIP    string

				nodesNumber = 1
			)

			BeforeAll(func() {
				containerIP, _ = setupEnv(&tempDirPath, &cpVIP, &clusterName, manifestValues, localIPv4, localIPv6, imagePath, configPath, k8sImagePath,
					e2e.DualstackFamilyIPv6, e2e.IPv6Family, []string{e2e.IPv6Family}, &client, &gobgpPeers, v129, kubeVIPBGPManifestTemplate, &gobgpClient,
					logger, nodesNumber, "auto_sourceif", "mpbgp-if-ipv6")
			})

			AfterAll(func() {
				for _, n := range gobgpPeers {
					_, err := gobgpClient.DeletePeer(context.TODO(), &api.DeletePeerRequest{
						Address: n.IP,
					})
					Expect(err).ToNot(HaveOccurred())
				}
				cleanupCluster(clusterName, tempDirPath, ConfigMtx, logger)
			})

			DescribeTable("advertise IPv4 routes over IPv6 session",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName, trafficPolicy, client, 1, gobgpClient, containerIP)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)

			DescribeTable("only stops advertising route if it was referenced by multiple services and all of them were deleted",
				func(svcName string, offset uint, trafficPolicy corev1.ServiceExternalTrafficPolicy) {
					testBGP(offset, e2e.IPv4Family, api.Family_AFI_IP, []corev1.IPFamily{corev1.IPv4Protocol}, svcName, trafficPolicy, client, 2, gobgpClient, containerIP)
				},
				Entry("with external traffic policy - cluster", "test-svc-cluster", SOffset.Get(), corev1.ServiceExternalTrafficPolicyCluster),
				Entry("with external traffic policy - local", "test-svc-local", SOffset.Get(), corev1.ServiceExternalTrafficPolicyLocal),
			)
		})
	}
})

func testBGP(offset uint, lbFamily string, afiFamily api.Family_Afi, svcFamily []corev1.IPFamily, svcName string,
	trafficPolicy corev1.ServiceExternalTrafficPolicy, client kubernetes.Interface, numberOfServices int, gobgpClient api.GobgpApiClient, expectedNexthop string) {
	lbAddress := e2e.GenerateVIP(lbFamily, offset)
	routeCheckFamily := &api.Family{
		Afi:  afiFamily,
		Safi: api.Family_SAFI_UNICAST,
	}
	testServiceBGP(svcName, lbAddress, trafficPolicy, client, svcFamily, numberOfServices, gobgpClient, routeCheckFamily, expectedNexthop)
}

func setupEnv(tempDirPath, cpVIP, clusterName *string, manifestValues *e2e.KubevipManifestValues,
	localIPv4, localIPv6, imagePath, configPath, k8sImagePath, clusterAddrFamily, bgpClientAddrFamily string, peerAddrFamily []string, client *kubernetes.Interface,
	gobgpPeers *[]*e2e.BGPPeerValues, v129 bool, kubeVIPBGPManifestTemplate *template.Template, gobgpClient *api.GobgpApiClient,
	logger log.Logger, nodesNumber int, mpbgpnexthop, clusterNameSuffix string) (string, string) {
	var err error
	*tempDirPath, err = os.MkdirTemp("", "kube-vip-test")
	Expect(err).ToNot(HaveOccurred())

	*cpVIP = e2e.GenerateVIP(clusterAddrFamily, SOffset.Get())

	var clusterIPFamily kindconfigv1alpha4.ClusterIPFamily
	var podSubnet, serviceSubnet string
	switch clusterAddrFamily {
	case e2e.IPv6Family:
		clusterIPFamily = kindconfigv1alpha4.IPv6Family
	case e2e.DualstackFamily:
		clusterIPFamily = kindconfigv1alpha4.DualStackFamily
	case e2e.DualstackFamilyIPv6:
		clusterIPFamily = kindconfigv1alpha4.DualStackFamily
		podSubnet = "fd00:10:244::/56,10.244.0.0/16"
		serviceSubnet = "fd00:10:96::/112,10.96.0.0/16"
	default:
		clusterIPFamily = kindconfigv1alpha4.IPv4Family
	}

	networking := &kindconfigv1alpha4.Networking{
		IPFamily: clusterIPFamily,
	}

	if podSubnet != "" && serviceSubnet != "" {
		networking.PodSubnet = podSubnet
		networking.ServiceSubnet = serviceSubnet
	}

	kvPeers := []*e2e.BGPPeerValues{}
	if slices.Contains(peerAddrFamily, e2e.IPv4Family) {
		kvPeers = append(kvPeers, &e2e.BGPPeerValues{
			IP:       localIPv4,
			AS:       goBGPAS,
			IPFamily: e2e.IPv4Family,
		})
	}

	if slices.Contains(peerAddrFamily, e2e.IPv6Family) {
		kvPeers = append(kvPeers, &e2e.BGPPeerValues{
			IP:       localIPv6,
			AS:       goBGPAS,
			IPFamily: e2e.IPv6Family,
		})
	}

	kvPeersStr := []string{}
	for _, p := range kvPeers {
		kvPeersStr = append(kvPeersStr, p.String())
	}

	manifestValues = &e2e.KubevipManifestValues{
		ControlPlaneVIP:    *cpVIP,
		ImagePath:          imagePath,
		ConfigPath:         configPath,
		ControlPlaneEnable: "false",
		SvcEnable:          "true",
		SvcElectionEnable:  "false",
		BGPAS:              kubevipAS,
		BGPPeers:           strings.Join(kvPeersStr, ","),
		MPBGPNexthop:       mpbgpnexthop,
		MPBGPNexthopIPv4:   defaultFixedNexthopv4,
		MPBGPNexthopIPv6:   defaultFixedNexthopv6,
	}

	By(manifestValues.BGPPeers)

	*clusterName, *client = prepareCluster(*tempDirPath, clusterNameSuffix, k8sImagePath, v129, kubeVIPBGPManifestTemplate, logger, manifestValues, networking, nodesNumber)

	container := fmt.Sprintf("%s-control-plane", *clusterName)

	containerIPv4, containerIPv6, err := GetContainerIPs(container)
	Expect(err).ToNot(HaveOccurred())

	if slices.Contains(peerAddrFamily, e2e.IPv4Family) {
		*gobgpPeers = append(*gobgpPeers, &e2e.BGPPeerValues{
			IP:       containerIPv4,
			AS:       kubevipAS,
			IPFamily: e2e.IPv4Family,
		})
	}

	if slices.Contains(peerAddrFamily, e2e.IPv6Family) {
		*gobgpPeers = append(*gobgpPeers, &e2e.BGPPeerValues{
			IP:       containerIPv6,
			AS:       kubevipAS,
			IPFamily: e2e.IPv6Family,
		})
	}

	if bgpClientAddrFamily == e2e.IPv6Family {
		*gobgpClient, err = newGoBGPClient(localIPv6, goBGPPort)
	} else {
		*gobgpClient, err = newGoBGPClient(localIPv4, goBGPPort)
	}
	Expect(err).ToNot(HaveOccurred())

	for _, p := range *gobgpPeers {
		if slices.Contains(peerAddrFamily, p.IPFamily) {
			peerCtx := context.TODO()
			Eventually(peerCtx, func() error {
				_, err = (*gobgpClient).AddPeer(context.TODO(), &api.AddPeerRequest{
					Peer: &api.Peer{
						Conf: &api.PeerConf{
							NeighborAddress: p.IP,
							PeerAsn:         uint32(p.AS),
						},
						AfiSafis: []*api.AfiSafi{
							{
								Config: &api.AfiSafiConfig{
									Enabled: true,
									Family: &api.Family{
										Afi:  api.Family_AFI_IP6,
										Safi: api.Family_SAFI_UNICAST,
									},
								},
							},
							{
								Config: &api.AfiSafiConfig{
									Enabled: true,
									Family: &api.Family{
										Afi:  api.Family_AFI_IP,
										Safi: api.Family_SAFI_UNICAST,
									},
								},
							},
						},
					},
				})
				return err
			}, "120s", "100ms").Should(Succeed())
		}
	}

	return containerIPv4, containerIPv6
}

func testServiceBGP(svcName, lbAddress string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceAddrFamily []corev1.IPFamily, numberOfServices int,
	gobgpClient api.GobgpApiClient, gobgpFamily *api.Family, expectedNexthop string) {
	lbAddresses := vip.Split(lbAddress)

	services := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
	}

	for _, svc := range services {
		createTestService(svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, serviceAddrFamily, trafficPolicy)
	}

	for _, addr := range lbAddresses {
		paths := checkGoBGPPaths(context.Background(), gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
		Expect(strings.Contains(paths[0].Prefix, lbAddress)).To(BeTrue())
		if expectedNexthop != "" {
			Expect(strings.Contains(paths[0].String(), fmt.Sprintf("next_hop:\"%s\"", expectedNexthop)) || strings.Contains(paths[0].String(), fmt.Sprintf("next_hops:\"%s\"", expectedNexthop))).To(BeTrue())
		}
	}

	for i := range numberOfServices {
		err := client.CoreV1().Services(dsNamespace).Delete(context.TODO(), services[i], metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
		if i < numberOfServices-1 {
			for _, addr := range lbAddresses {
				paths := checkGoBGPPaths(context.Background(), gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 1)
				Expect(strings.Contains(paths[0].Prefix, lbAddress)).To(BeTrue())
				if expectedNexthop != "" {
					Expect(strings.Contains(paths[0].String(), fmt.Sprintf("next_hop:\"%s\"", expectedNexthop)) || strings.Contains(paths[0].String(), fmt.Sprintf("next_hops:\"%s\"", expectedNexthop))).To(BeTrue())
				}
			}
		}
	}

	for _, addr := range lbAddresses {
		checkGoBGPPaths(context.Background(), gobgpClient, gobgpFamily, []*api.TableLookupPrefix{{Prefix: addr}}, 0)
	}
}

func GetContainerIPs(containerName string) (string, string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		panic(err)
	}
	containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to list containers: %w", err)
	}

	for _, c := range containers {
		for _, n := range c.Names {
			if n[1:] == containerName {
				fmt.Println(n)
				for _, n := range c.NetworkSettings.Networks {
					return n.IPAddress, n.GlobalIPv6Address, nil
				}
			}
		}
	}

	return "", "", nil
}

func newGoBGPClient(address string, port uint32) (api.GobgpApiClient, error) {
	grpcOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	target := net.JoinHostPort(address, strconv.Itoa(int(port)))
	conn, err := grpc.NewClient(target, grpcOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GoBGP server %q: %w", target, err)
	}

	return api.NewGobgpApiClient(conn), nil
}

func checkGoBGPPaths(ctx context.Context, client api.GobgpApiClient, family *api.Family, prefixes []*api.TableLookupPrefix, expectedPaths int) []*api.Destination {
	var paths []*api.Destination
	Eventually(func() error {
		var err error
		paths, err = getGoBGPPaths(ctx, client, family, prefixes)
		if err != nil {
			return err
		}
		if len(paths) != expectedPaths {
			return fmt.Errorf("expected %d paths, but found %d", expectedPaths, len(paths))
		}
		return nil
	}, "120s").ShouldNot(HaveOccurred())
	return paths
}

func getGoBGPPaths(ctx context.Context, client api.GobgpApiClient, family *api.Family, prefixes []*api.TableLookupPrefix) ([]*api.Destination, error) {
	pathCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	stream, err := client.ListPath(pathCtx, &api.ListPathRequest{
		TableType: api.TableType_GLOBAL,
		Family:    family,
		Name:      "",
		Prefixes:  prefixes,
		SortType:  api.ListPathRequest_PREFIX,
	})
	if err != nil {
		return nil, err
	}

	rib := make([]*api.Destination, 0)
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		rib = append(rib, r.Destination)
	}

	return rib, nil
}

func startGoBGP(config string, kill chan any) {
	By("starting GoBGP server")
	cmd := exec.Command("../../bin/gobgpd", "-f", config)
	go cmd.Run()
	<-kill
	By("stopping GoBGP server")
	err := cmd.Process.Kill()
	Expect(err).ToNot(HaveOccurred())
}
