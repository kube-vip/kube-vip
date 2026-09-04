package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLoopMetricsRegisterAndTrackLiveness(t *testing.T) {
	WatcherLoops.Reset()
	ElectionLoops.Reset()
	t.Cleanup(func() {
		WatcherLoops.Reset()
		ElectionLoops.Reset()
	})

	registry := prometheus.NewRegistry()
	registry.MustRegister(WatcherLoops, ElectionLoops)

	watcher := WatcherLoops.WithLabelValues("service")
	election := ElectionLoops.WithLabelValues("kubernetes")

	watcher.Inc()
	election.Inc()
	if got := testutil.ToFloat64(watcher); got != 1 {
		t.Fatalf("watcher loop gauge is %v, want 1", got)
	}
	if got := testutil.ToFloat64(election); got != 1 {
		t.Fatalf("election loop gauge is %v, want 1", got)
	}

	watcher.Dec()
	election.Dec()
	if got := testutil.ToFloat64(watcher); got != 0 {
		t.Fatalf("watcher loop gauge is %v after stop, want 0", got)
	}
	if got := testutil.ToFloat64(election); got != 0 {
		t.Fatalf("election loop gauge is %v after stop, want 0", got)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering loop gauges: %v", err)
	}
	seen := make(map[string]bool, len(families))
	for _, family := range families {
		seen[family.GetName()] = true
	}
	for _, name := range []string{"kube_vip_watcher_loops", "kube_vip_election_loops"} {
		if !seen[name] {
			t.Errorf("loop gauge %q was not registered", name)
		}
	}
}

func TestDataplaneMetricsRegister(t *testing.T) {
	VIPAddresses.Reset()
	VIPOperationsTotal.Reset()
	ARPAdvertisementsTotal.Reset()
	NDPAdvertisementsTotal.Reset()
	RouteOperationsTotal.Reset()
	DNSResolutionsTotal.Reset()
	t.Cleanup(func() {
		VIPAddresses.Reset()
		VIPOperationsTotal.Reset()
		ARPAdvertisementsTotal.Reset()
		NDPAdvertisementsTotal.Reset()
		RouteOperationsTotal.Reset()
		DNSResolutionsTotal.Reset()
	})

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		VIPAddresses,
		VIPOperationsTotal,
		ARPAdvertisementsTotal,
		NDPAdvertisementsTotal,
		RouteOperationsTotal,
		DNSResolutionsTotal,
		DNSIPChangesTotal,
	)

	VIPAddresses.WithLabelValues("eth0", "IPv4").Inc()
	VIPOperationsTotal.WithLabelValues("add", "ok").Inc()
	ARPAdvertisementsTotal.WithLabelValues("ok").Inc()
	NDPAdvertisementsTotal.WithLabelValues("error").Inc()
	RouteOperationsTotal.WithLabelValues("replace", "ok").Inc()
	DNSResolutionsTotal.WithLabelValues("error").Inc()
	DNSIPChangesTotal.Inc()

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering dataplane metrics: %v", err)
	}
	seen := make(map[string]bool, len(families))
	for _, family := range families {
		seen[family.GetName()] = true
	}
	for _, name := range []string{
		"kube_vip_vip_addresses",
		"kube_vip_vip_operations_total",
		"kube_vip_arp_advertisements_total",
		"kube_vip_ndp_advertisements_total",
		"kube_vip_route_operations_total",
		"kube_vip_dns_resolutions_total",
		"kube_vip_dns_ip_changes_total",
	} {
		if !seen[name] {
			t.Errorf("dataplane metric %q was not registered", name)
		}
	}
}
