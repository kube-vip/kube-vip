//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

const (
	matrixClusterNodes   = 1
	matrixMetricTimeout  = 120 * time.Second
	matrixMetricInterval = 2 * time.Second
	matrixMetricGap      = time.Second
)

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
		assertMatrixBackendsReady(ctx, deployment.cluster.Client, serviceName, combo.Provider)
		assertMatrixServiceVIP(ctx, deployment.cluster.Client, serviceName, serviceVIP)
		if combo.Mode == matrix.ModeBGP {
			assertMatrixBGPVIP(ctx, deployment.bgpClient, serviceVIP)
			assertMatrixBGPConnection(ctx, deployment.bgpClient, serviceVIP, "http", "80", "")
		}
		if combo.Mode == matrix.ModeARP {
			for _, address := range strings.Split(serviceVIP, ",") {
				assertConnection("http", address, "80", "", 5*time.Second, 120*time.Second)
			}
		} else if combo.Mode == matrix.ModeRT {
			assertMatrixRoutes(deployment, serviceVIP)
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

	cpVIP := matrixControlPlaneVIP(combo.Family, SOffset.Get())
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
		PrometheusHTTPServer:       ":2112",
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
			bgpPeers = append(bgpPeers, &e2e.BGPPeerValues{IP: peerIP, AS: bgp.GoBGPAS, Port: bgp.GoBGPPort, IPFamily: family})
		}
		manifestValues.BGPAS = bgp.KubevipAS
		manifestValues.BGPPeers = bgp.PeerStrings(bgpPeers)
	}

	templateName := ""
	if combo.Mode == matrix.ModeRT {
		templateName = "kube-vip-routing-table.yaml.tmpl"
	}
	deployment.cluster = e2e.CreateCluster(ctx, &e2e.ClusterSpec{
		Name:           fmt.Sprintf("matrix-%d-p%d", SOffset.Get(), GinkgoParallelProcess()),
		Nodes:          matrixClusterNodes,
		Networking:     networking,
		KubeVip:        manifestValues,
		Logger:         e2e.TestLogger{},
		ConfigMtx:      ConfigMtx,
		KubeadmPatches: matrixKubeadmPatches(cpVIP, hasCP),
		TemplateName:   templateName,
		UseDaemonSet:   combo.Shape == matrix.ShapeDaemonSet,
	})

	if combo.Mode == matrix.ModeBGP {
		deployment.bgpPeers = sharedBGPServer.AddClusterPeers(ctx, deployment.cluster.Nodes, bgp.KubevipAS, matrixBGPPeerFamilies(combo.Family))
		deployment.bgpClient = sharedBGPServer.Client
		Expect(sharedBGPServer.WaitForEstablished(ctx, deployment.bgpPeers)).To(Succeed())
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
			assertMatrixBGPConnection(ctx, deployment.bgpClient, address, "https", "6443", "livez")
			continue
		}
		if combo.Mode == matrix.ModeRT {
			assertMatrixRoutes(deployment, address)
			continue
		}
		assertControlPlaneIsRoutable(address, 5*time.Second, 120*time.Second)
	}
}

func assertMatrixBGPConnection(ctx context.Context, client api.GoBgpServiceClient, addresses, protocol, port, suffix string) {
	for _, address := range strings.Split(addresses, ",") {
		var nextHops []string
		Eventually(func() error {
			nextHops = bgp.ResolveVIP(ctx, client, address)
			if len(nextHops) == 0 {
				return fmt.Errorf("BGP has no next hop for VIP %s", address)
			}
			return nil
		}, "120s", "2s").Should(Succeed())

		prefix := address + "/32"
		familyArg := "-4"
		if net.ParseIP(address).To4() == nil {
			prefix = address + "/128"
			familyArg = "-6"
		}
		cmd := exec.Command("sudo", "ip", familyArg, "route", "replace", prefix, "via", nextHops[0])
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "install route to BGP VIP %s via %s: %s", address, nextHops[0], strings.TrimSpace(string(output)))
		DeferCleanup(func() {
			output, err := exec.Command("sudo", "ip", familyArg, "route", "del", prefix).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "remove route to BGP VIP %s: %s", address, strings.TrimSpace(string(output)))
		})
		assertConnection(protocol, address, port, suffix, 5*time.Second, 120*time.Second)
	}
}

func assertMatrixRoutes(deployment *matrixDeployment, addresses string) {
	for _, node := range deployment.cluster.Nodes {
		for _, address := range strings.Split(addresses, ",") {
			present, output, err := e2e.CheckRoutePresence(address, node.String(), true)
			Expect(err).NotTo(HaveOccurred(), "route output: %s", output)
			Expect(present).To(BeTrue(), "route output: %s", output)
		}
	}
}

func assertMatrixBackendsReady(ctx context.Context, client kubernetes.Interface, serviceName string, provider matrix.Provider) {
	Eventually(func() error {
		return matrixBackendsReady(ctx, client, dsNamespace, serviceName, provider)
	}, "120s", "2s").Should(Succeed())
}

func matrixBackendsReady(ctx context.Context, client kubernetes.Interface, namespace, serviceName string, provider matrix.Provider) error {
	if provider == matrix.ProviderEndpoints {
		endpoints, err := client.CoreV1().Endpoints(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		ready := 0
		for _, subset := range endpoints.Subsets {
			ready += len(subset.Addresses)
		}
		if ready == 0 {
			return fmt.Errorf("endpoints %s/%s have no ready addresses; subsets: %+v", namespace, serviceName, endpoints.Subsets)
		}
		return nil
	}

	slices, err := client.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + serviceName,
	})
	if err != nil {
		return err
	}
	ready := 0
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
				ready += len(endpoint.Addresses)
			}
		}
	}
	if ready == 0 {
		return fmt.Errorf("endpoint slices for %s/%s have no ready addresses; slices: %+v", namespace, serviceName, slices.Items)
	}
	return nil
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
	nodes := matrixNodeNames(deployment)
	if shouldAssertMatrixLeader(combo, hasService) {
		if !hasMetricCapability(ctx, deployment.cluster.Name, nodes, "kube_vip_is_leader") {
			return
		}
		leaseName := matrixLeaderLease(combo, serviceName)
		labels := map[string]string{"lease_name": leaseName}
		assertMetricValue := func() (float64, error) {
			var total float64
			for _, node := range nodes {
				metrics, err := e2e.ScrapeMetrics(ctx, deployment.cluster.Name, node)
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
		if !hasMetricCapability(ctx, deployment.cluster.Name, nodes, "kube_vip_active_services") {
			return
		}
		activeServices := func() (float64, error) {
			var total float64
			for _, node := range nodes {
				metrics, err := e2e.ScrapeMetrics(ctx, deployment.cluster.Name, node)
				if err != nil {
					return 0, err
				}
				total += e2e.SumMetric(metrics, "kube_vip_active_services", map[string]string{
					"namespace": dsNamespace,
				})
			}
			return total, nil
		}
		Eventually(activeServices, matrixMetricTimeout, matrixMetricInterval).Should(Equal(float64(1)))
		Consistently(activeServices, 10*time.Second, matrixMetricInterval).Should(Equal(float64(1)))
	}

	assertMatrixLoopMetrics(ctx, deployment, combo, hasService, nodes)
}

func matrixNodeNames(deployment *matrixDeployment) []string {
	nodes := make([]string, 0, len(deployment.cluster.Nodes))
	for _, node := range deployment.cluster.Nodes {
		nodes = append(nodes, node.String())
	}
	return nodes
}

type matrixLoopMetric struct {
	name     string
	labels   map[string]string
	expected float64
}

func assertMatrixLoopMetrics(ctx context.Context, deployment *matrixDeployment, combo matrix.Combo, hasService bool, nodes []string) {
	for _, assertion := range matrixLoopMetrics(combo, hasService) {
		if !hasMetricCapabilityWithLabels(ctx, deployment.cluster.Name, nodes, assertion.name, assertion.labels) {
			continue
		}
		for _, node := range nodes {
			assertEventuallyStableMetric(
				ctx,
				deployment.cluster.Name,
				node,
				assertion.name,
				assertion.labels,
				assertion.expected,
				matrixMetricTimeout,
				matrixMetricInterval,
				matrixMetricGap,
			)
		}
	}
}

func matrixLoopMetrics(combo matrix.Combo, hasService bool) []matrixLoopMetric {
	assertions := make([]matrixLoopMetric, 0, 4)
	if hasService {
		serviceWatchers := float64(1)
		// On-demand mode runs the forced per-service watcher alongside the
		// regular service watcher. The latter may ignore this annotated
		// service, but its watcher loop is still live.
		if combo.Election == matrix.ElectionOnDemand {
			serviceWatchers = 2
		}
		assertions = append(assertions,
			matrixLoopMetric{
				name:     "kube_vip_watcher_loops",
				labels:   map[string]string{"kind": "service"},
				expected: serviceWatchers,
			},
			matrixLoopMetric{
				name:     "kube_vip_watcher_loops",
				labels:   map[string]string{"kind": "endpoint"},
				expected: 1,
			},
		)
		if combo.Election == matrix.ElectionPerService {
			assertions = append(assertions, matrixLoopMetric{
				name:     "kube_vip_watcher_loops",
				labels:   map[string]string{"kind": "lease"},
				expected: 1,
			})
		}
	}

	electionLoops := 0
	// ARP's control-plane worker always uses the control-plane election. BGP
	// disables it in its manifest, while routing-table control-plane startup
	// directly serves the VIP and does not create an election loop.
	if combo.Mode == matrix.ModeARP && (combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth) {
		electionLoops++
	}
	if hasService {
		switch combo.Mode {
		case matrix.ModeARP:
			if combo.Election == matrix.ElectionPerService {
				electionLoops++
			} else {
				// ARP uses GlobalLeader for every non per-service service
				// arrangement, including the no-election matrix value.
				electionLoops++
			}
		case matrix.ModeBGP:
			if combo.Election == matrix.ElectionPerService || combo.Election == matrix.ElectionOnDemand {
				electionLoops++
			}
		case matrix.ModeRT:
			switch combo.Election {
			case matrix.ElectionGlobal, matrix.ElectionPerService:
				electionLoops++
			case matrix.ElectionOnDemand:
				electionLoops++
				if combo.Function == matrix.FunctionBoth {
					electionLoops++
				}
			}
		}
	}
	if electionLoops > 0 {
		assertions = append(assertions, matrixLoopMetric{
			name:     "kube_vip_election_loops",
			labels:   map[string]string{"type": "kubernetes"},
			expected: float64(electionLoops),
		})
	}
	return assertions
}

func shouldAssertMatrixLeader(combo matrix.Combo, hasService bool) bool {
	if combo.Election == matrix.ElectionNone {
		return false
	}
	if combo.Mode == matrix.ModeBGP {
		return hasService
	}
	if combo.Mode == matrix.ModeRT {
		return hasService && combo.Election != matrix.ElectionNone
	}
	if combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth {
		return true
	}
	return hasService
}

func matrixLeaderLease(combo matrix.Combo, serviceName string) string {
	if combo.Mode == matrix.ModeARP && (combo.Function == matrix.FunctionCP || combo.Function == matrix.FunctionBoth) {
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

func matrixControlPlaneVIP(family matrix.Family, offset uint) string {
	if family == matrix.FamilyDual {
		// The dual-stack Kind configuration is IPv6-primary, so kubeadm's API
		// endpoint and kube-vip must use the IPv6 member of the VIP pair.
		return e2e.GenerateVIP(utils.IPv6Family, offset, defaultNetwork)
	}
	return matrixVIP(family, offset)
}

func matrixKubeadmPatches(cpVIP string, enabled bool) []kindconfigv1alpha4.PatchJSON6902 {
	if !enabled {
		return nil
	}
	return []kindconfigv1alpha4.PatchJSON6902{{
		Group: "kubeadm.k8s.io", Version: "v1beta3", Kind: "ClusterConfiguration",
		Patch: fmt.Sprintf("- op: add\n  path: /apiServer/certSANs/-\n  value: %q", cpVIP),
	}}
}
