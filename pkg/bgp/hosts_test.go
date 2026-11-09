package bgp

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestAddHostDoesNotTrackFailedAdvertisement(t *testing.T) {
	server := newPeerTestServer(t, kubevip.BGPConfig{})
	const invalidAddress = "not-a-cidr"

	if err := server.AddHost(context.Background(), invalidAddress, "service-a"); err == nil {
		t.Fatal("AddHost returned no error for an invalid address")
	}
	if _, exists := server.tracker[invalidAddress]; exists {
		t.Fatal("failed advertisement poisoned host tracker")
	}
	if err := server.AddHost(context.Background(), invalidAddress, "service-b"); err == nil {
		t.Fatal("second AddHost bypassed advertisement after prior failure")
	}
}
