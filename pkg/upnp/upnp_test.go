package upnp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/huin/goupnp"
	"github.com/kube-vip/kube-vip/pkg/upnp"
)

var (
	_ func(context.Context) []upnp.ConnectionClient = upnp.GetConnectionClients
	_ func(context.Context) []upnp.Gateway          = upnp.GetGatewayClients
)

type legacyConnectionClient struct {
	mappingCalls int
	addressCalls int
}

func (c *legacyConnectionClient) AddPortMapping(string, uint16, string, uint16, string, bool, string, uint32) error {
	c.mappingCalls++
	return nil
}

func (c *legacyConnectionClient) GetExternalIPAddress() (string, error) {
	c.addressCalls++
	return "198.51.100.1", nil
}

func (c *legacyConnectionClient) GetServiceClient() *goupnp.ServiceClient {
	return nil
}

func TestLegacyConnectionClient(t *testing.T) {
	client := &legacyConnectionClient{}
	gateway := upnp.Gateway{ConnectionClient: client}
	mapPort := func(ctx context.Context) error {
		return upnp.AddPortMapping(ctx, gateway.ConnectionClient, "0.0.0.0", 80, "TCP", 80, "192.0.2.2", true, "web", 300)
	}

	if err := mapPort(t.Context()); err != nil {
		t.Fatalf("map port with a legacy client: %v", err)
	}
	if ip, err := upnp.GetExternalIPAddress(t.Context(), gateway.ConnectionClient); err != nil || ip != "198.51.100.1" {
		t.Fatalf("legacy client address lookup: got %q, %v", ip, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := mapPort(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("map port after cancellation: got %v", err)
	}
	if _, err := upnp.GetExternalIPAddress(ctx, gateway.ConnectionClient); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup address after cancellation: got %v", err)
	}
	if client.mappingCalls != 1 || client.addressCalls != 1 {
		t.Fatalf("legacy method calls: got %d mappings and %d lookups, want one each", client.mappingCalls, client.addressCalls)
	}
}
