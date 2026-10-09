package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBuildInfoScrape(t *testing.T) {
	BuildInfo.Reset()
	t.Cleanup(BuildInfo.Reset)

	BuildInfo.WithLabelValues("v1.2.3", "abc123", "node-a").Set(1)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(BuildInfo)

	request := httptest.NewRequest("GET", "/metrics", nil)
	response := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("scrape status = %d, want 200", response.Code)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading scrape: %v", err)
	}
	if err := testutil.GatherAndCompare(registry, strings.NewReader(`# HELP kube_vip_build_info Constant 1; track version skew across nodes
# TYPE kube_vip_build_info gauge
kube_vip_build_info{build="abc123",node="node-a",version="v1.2.3"} 1
`), "kube_vip_build_info"); err != nil {
		t.Fatalf("scraping build info: %v", err)
	}
	if !strings.Contains(string(body), `kube_vip_build_info{build="abc123",node="node-a",version="v1.2.3"} 1`) {
		t.Fatalf("scrape does not contain build info: %s", body)
	}
}

func TestTrackServiceElectionLoopDeletesOnlyAfterFinalExit(t *testing.T) {
	ServiceElectionLoops.Reset()
	t.Cleanup(ServiceElectionLoops.Reset)

	doneFirst := TrackServiceElectionLoop("default", "api")
	doneSecond := TrackServiceElectionLoop("default", "api")
	if got := testutil.ToFloat64(ServiceElectionLoops.WithLabelValues("default", "api")); got != 2 {
		t.Fatalf("loop gauge = %v, want 2", got)
	}

	doneFirst()
	if got := testutil.ToFloat64(ServiceElectionLoops.WithLabelValues("default", "api")); got != 1 {
		t.Fatalf("loop gauge after first exit = %v, want 1", got)
	}
	doneSecond()

	registry := prometheus.NewRegistry()
	registry.MustRegister(ServiceElectionLoops)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering loop gauge: %v", err)
	}
	if len(families) != 0 {
		t.Fatalf("loop series remains after final exit: %v", families)
	}
}

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
