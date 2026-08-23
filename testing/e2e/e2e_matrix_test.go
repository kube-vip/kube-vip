//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/e2e/bgp"
	"github.com/kube-vip/kube-vip/testing/e2e/matrix"
)

const matrixClusterNodes = 1

type matrixDeployment struct {
	cluster   *e2e.Cluster
	cpVIP     string
	bgpClient api.GoBgpServiceClient
	bgpPeers  []*e2e.BGPPeerValues
	bgpServer *bgp.Server
}

var _ = Describe("kube-vip pairwise combination matrix", Label("matrix"), func() {
	if Mode != ModeMatrix {
		return
	}
	tableArgs := []any{
		func(combo matrix.Combo) {
			runMatrixCombo(combo)
		},
	}
	tableArgs = append(tableArgs, matrixTableEntries()...)
	DescribeTable("deploys a valid pairwise combination and reaches steady state", tableArgs...)
})

func matrixTableEntries() []any {
	combos := matrix.Generate()
	shardIndex, shardCount := matrixShard()
	entries := make([]any, 0, len(combos))
	for i, combo := range combos {
		if shardCount > 1 && i%shardCount != shardIndex-1 {
			continue
		}
		entries = append(entries, Entry(matrix.FormatValues(
			string(combo.Mode),
			string(combo.Function),
			string(combo.Family),
			string(combo.Election),
			string(combo.Shape),
			string(combo.Provider),
			string(combo.ETP),
		), combo))
	}
	return entries
}

func matrixShard() (int, int) {
	value := strings.TrimSpace(os.Getenv("MATRIX_SHARD"))
	if value == "" {
		return 1, 1
	}

	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		panic(fmt.Sprintf("MATRIX_SHARD must have the form i/n, got %q", value))
	}
	index, err := strconv.Atoi(parts[0])
	if err != nil {
		panic(fmt.Sprintf("MATRIX_SHARD has invalid shard index %q: %v", parts[0], err))
	}
	count, err := strconv.Atoi(parts[1])
	if err != nil {
		panic(fmt.Sprintf("MATRIX_SHARD has invalid shard count %q: %v", parts[1], err))
	}
	if index < 1 || count < 1 || index > count {
		panic(fmt.Sprintf("MATRIX_SHARD must satisfy 1 <= i <= n, got %q", value))
	}
	return index, count
}

func runMatrixCombo(combo matrix.Combo) {
	if combo.Mode == matrix.ModeWireGuard {
		Skip("WireGuard matrix entries are retained for pairwise coverage, but WireGuard has no e2e setup yet")
	}

	ctx := context.Background()
	deployment := createMatrixDeployment(ctx, combo)
	registerMatrixCleanup(ctx, deployment)

	hasCP := combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth
	hasService := combo.Function == matrix.FunctionSvc || combo.Function == matrix.FunctionBoth
	serviceName := ""
	if hasService {
		serviceName = fmt.Sprintf("matrix-svc-%d", SOffset.Get())
		backendName := fmt.Sprintf("matrix-backend-%d", SOffset.Get())
		createDS(ctx, backendName, dsNamespace, deployment.cluster.Client, 80)
		serviceVIP := matrixVIP(combo.Family, SOffset.Get())
		createTestService(ctx, serviceName, dsNamespace, backendName, serviceVIP,
			deployment.cluster.Client, corev1.IPFamilyPolicyPreferDualStack,
			matrixServiceFamilies(combo.Family), matrixTrafficPolicy(combo.ETP), "", 80, false,
			combo.Election == matrix.ElectionOnDemand)
		assertMatrixServiceVIP(ctx, deployment.cluster.Client, serviceName, serviceVIP)
		if combo.Mode == matrix.ModeBGP {
			assertMatrixBGPVIP(ctx, deployment.bgpClient, serviceVIP)
		}
	}

	if hasCP {
		assertMatrixControlPlaneVIP(ctx, deployment, combo)
	}

	assertMatrixMetrics(ctx, deployment, combo, serviceName)
}

func createMatrixDeployment(ctx context.Context, combo matrix.Combo) *matrixDeployment {
	clusterIPFamily, podSubnet, serviceSubnet := matrixClusterFamily(combo.Family)
	networking := kindconfigv1alpha4.Networking{IPFamily: clusterIPFamily}
	if podSubnet != "" {
		networking.PodSubnet = podSubnet
		networking.ServiceSubnet = serviceSubnet
	}

	cpVIP := matrixVIP(combo.Family, SOffset.Get())
	hasCP := combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth
	hasService := combo.Function == matrix.FunctionSvc || combo.Function == matrix.FunctionBoth
	manifestValues := e2e.KubevipManifestValues{
		ControlPlaneVIP:            cpVIP,
		ControlPlaneEnable:         strconv.FormatBool(hasCP),
		SvcEnable:                  strconv.FormatBool(hasService),
		SvcElectionEnable:          strconv.FormatBool(combo.Election == matrix.ElectionPerService),
		VipElectionEnable:          strconv.FormatBool(combo.Election == matrix.ElectionGlobal || (hasCP && combo.Election != matrix.ElectionNone)),
		EnableEndpoints:            strconv.FormatBool(combo.Provider == matrix.ProviderEndpoints),
		EnableNodeLabeling:         "false",
		EnableServiceSecurity:      "true",
		PerServiceElectionOnDemand: strconv.FormatBool(combo.Election == matrix.ElectionOnDemand),
		Mode:                       string(combo.Mode),
	}

	deployment := &matrixDeployment{cpVIP: cpVIP}
	if combo.Mode == matrix.ModeBGP {
		Expect(sharedBGPServer).NotTo(BeNil(), "matrix BGP combos require the shared GoBGP server")
		deployment.bgpServer = sharedBGPServer
		peerFamilies := matrixBGPPeerFamilies(combo.Family)
		bgpPeers := make([]*e2e.BGPPeerValues, 0, len(peerFamilies))
		for _, family := range peerFamilies {
			peerIP := sharedBGPServer.LocalIPv4
			if family == utils.IPv6Family {
				peerIP = sharedBGPServer.LocalIPv6
			}
			bgpPeers = append(bgpPeers, &e2e.BGPPeerValues{IP: peerIP, AS: bgp.GoBGPAS, IPFamily: family})
		}
		manifestValues.BGPAS = bgp.KubevipAS
		manifestValues.BGPPeers = bgp.PeerStrings(bgpPeers)
	}

	templateName := ""
	if combo.Mode == matrix.ModeRT {
		templateName = "kube-vip-routing-table.yaml.tmpl"
	}
	deployment.cluster = e2e.CreateCluster(ctx, &e2e.ClusterSpec{
		Name:         fmt.Sprintf("matrix-%d-p%d", SOffset.Get(), GinkgoParallelProcess()),
		Nodes:        matrixClusterNodes,
		Networking:   networking,
		KubeVip:      manifestValues,
		Logger:       e2e.TestLogger{},
		ConfigMtx:    ConfigMtx,
		TemplateName: templateName,
		UseDaemonSet: combo.Shape == matrix.ShapeDaemonSet,
	})

	if combo.Mode == matrix.ModeBGP {
		deployment.bgpPeers = sharedBGPServer.AddClusterPeers(ctx, deployment.cluster.Nodes, bgp.KubevipAS, matrixBGPPeerFamilies(combo.Family))
		deployment.bgpClient = sharedBGPServer.Client
	}
	return deployment
}

func registerMatrixCleanup(ctx context.Context, deployment *matrixDeployment) {
	DeferCleanup(func() {
		if deployment.bgpServer != nil {
			deployment.bgpServer.RemovePeers(ctx, deployment.bgpPeers)
		}

		logDir, err := os.MkdirTemp("", "kube-vip-matrix-logs")
		Expect(err).NotTo(HaveOccurred())
		deployment.cluster.SaveLogs(ctx, logDir)
		if os.Getenv("E2E_KEEP_LOGS") != "true" {
			Expect(os.RemoveAll(logDir)).To(Succeed())
		}
		deployment.cluster.Delete()
	})
}

func assertMatrixControlPlaneVIP(ctx context.Context, deployment *matrixDeployment, combo matrix.Combo) {
	for _, address := range strings.Split(deployment.cpVIP, ",") {
		if combo.Mode == matrix.ModeBGP {
			assertMatrixBGPVIP(ctx, deployment.bgpClient, address)
			continue
		}
		assertControlPlaneIsRoutable(address, 5*time.Second, 120*time.Second)
	}
}

func assertMatrixServiceVIP(ctx context.Context, client kubernetes.Interface, name, vipAddress string) {
	addresses := strings.Split(vipAddress, ",")
	Eventually(func() error {
		service, err := client.CoreV1().Services(dsNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, address := range addresses {
			found := false
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				if ingress.IP == address || ingress.Hostname == address {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("service %s/%s has not published VIP %s", dsNamespace, name, address)
			}
		}
		return nil
	}, "120s", "2s").Should(Succeed())
}

func assertMatrixBGPVIP(ctx context.Context, client api.GoBgpServiceClient, vipAddress string) {
	if client == nil {
		return
	}
	for _, address := range strings.Split(vipAddress, ",") {
		family := &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
		if parsed := net.ParseIP(address); parsed != nil && parsed.To4() == nil {
			family.Afi = api.Family_AFI_IP6
		}
		paths := bgp.CheckPaths(ctx, client, family, []*api.TableLookupPrefix{{Prefix: address}}, 1)
		Expect(paths).NotTo(BeEmpty())
	}
}

func assertMatrixMetrics(ctx context.Context, deployment *matrixDeployment, combo matrix.Combo, serviceName string) {
	hasService := serviceName != ""
	if shouldAssertMatrixLeader(combo, hasService) {
		leaseName := matrixLeaderLease(combo, serviceName)
		labels := map[string]string{"lease_name": leaseName}
		assertMetricValue := func() (float64, error) {
			var total float64
			for _, node := range deployment.cluster.Nodes {
				metrics, err := e2e.ScrapeMetrics(deployment.cluster.Name, node.String())
				if err != nil {
					return 0, err
				}
				total += e2e.SumMetric(metrics, "kube_vip_is_leader", labels)
			}
			return total, nil
		}
		Eventually(assertMetricValue, "120s", "2s").Should(Equal(float64(1)))
		Consistently(assertMetricValue, "10s", "2s").Should(Equal(float64(1)))
	}

	if hasService {
		activeServices := func() (float64, error) {
			var total float64
			for _, node := range deployment.cluster.Nodes {
				metrics, err := e2e.ScrapeMetrics(deployment.cluster.Name, node.String())
				if err != nil {
					return 0, err
				}
				total += e2e.SumMetric(metrics, "kube_vip_active_services", map[string]string{
					"namespace": dsNamespace,
				})
			}
			return total, nil
		}
		Eventually(activeServices, "120s", "2s").Should(BeNumerically(">=", float64(1)))
		Consistently(activeServices, "10s", "2s").Should(BeNumerically(">=", float64(1)))
	}
}

func shouldAssertMatrixLeader(combo matrix.Combo, hasService bool) bool {
	if combo.Election == matrix.ElectionNone {
		return false
	}
	if combo.Mode == matrix.ModeBGP {
		return hasService && combo.Election != matrix.ElectionGlobal
	}
	if combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth {
		return true
	}
	return hasService
}

func matrixLeaderLease(combo matrix.Combo, serviceName string) string {
	if combo.Mode != matrix.ModeBGP && (combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth) {
		return "plndr-cp-lock"
	}
	if combo.Election == matrix.ElectionGlobal {
		return "plndr-svcs-lock"
	}
	return "kubevip-" + serviceName
}

func matrixClusterFamily(family matrix.Family) (kindconfigv1alpha4.ClusterIPFamily, string, string) {
	switch family {
	case matrix.FamilyV6:
		return kindconfigv1alpha4.IPv6Family, "", ""
	case matrix.FamilyDual:
		return kindconfigv1alpha4.DualStackFamily, "fd00:10:244::/56,10.244.0.0/16", "fd00:10:96::/112,10.96.0.0/16"
	default:
		return kindconfigv1alpha4.IPv4Family, "", ""
	}
}

func matrixServiceFamilies(family matrix.Family) []corev1.IPFamily {
	switch family {
	case matrix.FamilyV6:
		return []corev1.IPFamily{corev1.IPv6Protocol}
	case matrix.FamilyDual:
		return []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}
	default:
		return []corev1.IPFamily{corev1.IPv4Protocol}
	}
}

func matrixBGPPeerFamilies(family matrix.Family) []string {
	switch family {
	case matrix.FamilyV6:
		return []string{utils.IPv6Family}
	case matrix.FamilyDual:
		return []string{utils.IPv4Family, utils.IPv6Family}
	default:
		return []string{utils.IPv4Family}
	}
}

func matrixTrafficPolicy(etp matrix.ETP) corev1.ServiceExternalTrafficPolicy {
	if etp == matrix.ETPLocal {
		return corev1.ServiceExternalTrafficPolicyLocal
	}
	return corev1.ServiceExternalTrafficPolicyCluster
}

func matrixVIP(family matrix.Family, offset uint) string {
	switch family {
	case matrix.FamilyV6:
		return e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
	case matrix.FamilyDual:
		return e2e.GenerateDualStackVIP(offset, defaultNetwork)
	default:
		return e2e.GenerateVIP(utils.IPv4Family, offset, defaultNetwork)
	}
}
