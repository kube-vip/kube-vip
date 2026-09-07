package wireguard

import "testing"

func TestTunnelManagerAcquireIsIdempotentForOwner(t *testing.T) {
	const vip = "192.0.2.10"
	manager := newSeededTunnelManager()

	for range 3 {
		if err := manager.AcquireTunnelForVIP(vip, "service-a"); err != nil {
			t.Fatalf("AcquireTunnelForVIP() error = %v", err)
		}
	}
	if got := manager.GetRefCount(vip); got != 1 {
		t.Fatalf("reference count after repeated acquire = %d, want 1", got)
	}

	if err := manager.ReleaseTunnelForVIP(vip, "service-a"); err != nil {
		t.Fatalf("ReleaseTunnelForVIP() error = %v", err)
	}
	if got := manager.GetRefCount(vip); got != 0 {
		t.Fatalf("reference count after release = %d, want 0", got)
	}
	if manager.GetTunnelForVIP(vip) != nil {
		t.Fatal("tunnel remained active after its only owner released it")
	}
}

func TestTunnelManagerRetainsSharedVIPUntilFinalOwnerReleased(t *testing.T) {
	const vip = "192.0.2.10"
	manager := newSeededTunnelManager()

	if err := manager.AcquireTunnelForVIP(vip, "service-a"); err != nil {
		t.Fatalf("first AcquireTunnelForVIP() error = %v", err)
	}
	if err := manager.AcquireTunnelForVIP(vip, "service-b"); err != nil {
		t.Fatalf("second AcquireTunnelForVIP() error = %v", err)
	}
	if got := manager.GetRefCount(vip); got != 2 {
		t.Fatalf("shared reference count = %d, want 2", got)
	}

	if err := manager.ReleaseTunnelForVIP(vip, "service-a"); err != nil {
		t.Fatalf("first ReleaseTunnelForVIP() error = %v", err)
	}
	if got := manager.GetRefCount(vip); got != 1 {
		t.Fatalf("reference count after first release = %d, want 1", got)
	}
	if manager.GetTunnelForVIP(vip) == nil {
		t.Fatal("shared tunnel was removed while one owner remained")
	}

	if err := manager.ReleaseTunnelForVIP(vip, "service-b"); err != nil {
		t.Fatalf("second ReleaseTunnelForVIP() error = %v", err)
	}
	if got := manager.GetRefCount(vip); got != 0 {
		t.Fatalf("reference count after final release = %d, want 0", got)
	}
	if manager.GetTunnelForVIP(vip) != nil {
		t.Fatal("shared tunnel remained active after its final owner released it")
	}
}

func TestTunnelManagerIgnoresUnknownOwnerRelease(t *testing.T) {
	const vip = "192.0.2.10"
	manager := newSeededTunnelManager()
	if err := manager.AcquireTunnelForVIP(vip, "current-service"); err != nil {
		t.Fatalf("AcquireTunnelForVIP() error = %v", err)
	}

	if err := manager.ReleaseTunnelForVIP(vip, "stale-service"); err != nil {
		t.Fatalf("ReleaseTunnelForVIP() error = %v", err)
	}
	if got := manager.GetRefCount(vip); got != 1 {
		t.Fatalf("reference count after stale release = %d, want 1", got)
	}
	if manager.GetTunnelForVIP(vip) == nil {
		t.Fatal("stale owner release removed the current owner's tunnel")
	}
}

func newSeededTunnelManager() *TunnelManager {
	manager := NewTunnelManager()
	manager.tunnels["192.0.2.10"] = NewWireGuard(WGConfig{InterfaceName: "kube-vip-test-missing"})
	return manager
}
