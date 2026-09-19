package worker

import (
	"net/netip"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/metrics"
	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveBGPPeerStateUsesRemotePort(t *testing.T) {
	metrics.BGPSessionInfoGauge.Reset()
	t.Cleanup(metrics.BGPSessionInfoGauge.Reset)

	observeBGPPeerState(&apiutil.WatchEventMessage_PeerEvent{
		Type: apiutil.PEER_EVENT_STATE,
		Peer: apiutil.Peer{
			State: apiutil.PeerState{
				NeighborAddress: netip.MustParseAddr("192.0.2.10"),
				SessionState:    5,
			},
			Transport: apiutil.Transport{RemotePort: 10179},
		},
	})

	if got := testutil.ToFloat64(metrics.BGPSessionInfoGauge.WithLabelValues(api.PeerState_SessionState_name[6], "192.0.2.10:10179")); got != 1 {
		t.Fatalf("custom-port BGP session metric = %v, want 1", got)
	}
}
