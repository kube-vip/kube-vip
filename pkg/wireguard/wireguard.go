package wireguard

import (
	"fmt"
	"net"
	"time"

	log "log/slog"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type WGConfig struct {
	InterfaceName string // e.g., "wg0"
	PrivateKey    string
	PeerPublicKey string
	PeerEndpoint  string
	AllowedIPs    []string // CIDRs allowed through tunnel
	ListenPort    int
	KeepAlive     time.Duration
	Routes        []string // CIDRs to route via wg
}

type WireGuard struct {
	cfg  WGConfig
	link netlink.Link
}

func NewWireGuard(cfg WGConfig) *WireGuard {
	return &WireGuard{
		cfg: cfg,
	}
}

// Up brings up the interface, configures peers, and sets routes
func (w *WireGuard) Up() error {
	if err := w.createInterface(); err != nil {
		return err
	}
	if err := w.configurePeer(); err != nil {
		w.teardown()
		return err
	}
	if err := w.addRoutes(); err != nil {
		w.teardown()
		return err
	}
	return nil
}

// Down tears down the interface and routes
func (w *WireGuard) Down() error {
	return w.teardown()
}

// CreateInterface creates the wg interface in the current netns if it doesn't exist
func (w *WireGuard) createInterface() error {
	genericLink := &netlink.GenericLink{
		LinkAttrs: netlink.LinkAttrs{Name: w.cfg.InterfaceName},
		LinkType:  "wireguard",
	}

	err := netlink.LinkAdd(genericLink)
	if err != nil && !isExistErr(err) {
		return fmt.Errorf("failed to create wireguard interface: %v", err)
	}

	// Set up interface
	link, err := netlink.LinkByName(w.cfg.InterfaceName)
	if err != nil {
		return fmt.Errorf("cannot find interface after creation: %v", err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring interface up: %v", err)
	}

	w.link = link
	log.Debug("create link ", "interface", w.cfg.InterfaceName)
	return nil
}

// ConfigurePeer sets the private key, peer, allowed IPs, endpoint, keepalive
func (w *WireGuard) configurePeer() error {
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("failed to open wgctrl client: %v", err)
	}
	defer client.Close()

	privKey, err := wgtypes.ParseKey(w.cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("invalid private key: %v", err)
	}

	pubKey, err := wgtypes.ParseKey(w.cfg.PeerPublicKey)
	if err != nil {
		return fmt.Errorf("invalid peer public key: %v", err)
	}

	addr, err := net.ResolveUDPAddr("udp", w.cfg.PeerEndpoint)
	if err != nil {
		return fmt.Errorf("Failed to resolve UDP address: %v", err)
	}

	peer := wgtypes.PeerConfig{
		PublicKey:                   pubKey,
		Endpoint:                    addr,
		PersistentKeepaliveInterval: &w.cfg.KeepAlive,
		ReplaceAllowedIPs:           true,
	}

	for _, cidr := range w.cfg.AllowedIPs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid AllowedIP %s: %v", cidr, err)
		}
		peer.AllowedIPs = append(peer.AllowedIPs, *ipnet)
	}

	conf := wgtypes.Config{
		PrivateKey:   &privKey,
		ListenPort:   &w.cfg.ListenPort,
		ReplacePeers: true,
		Peers:        []wgtypes.PeerConfig{peer},
	}

	if err := client.ConfigureDevice(w.cfg.InterfaceName, conf); err != nil {
		return fmt.Errorf("failed to configure wireguard device: %v", err)
	}

	log.Debug("wireguard device configured ", "interface", w.cfg.InterfaceName)
	return nil
}

// AddRoutes creates the routes for the VIP/public IPs via the wg interface
func (w *WireGuard) addRoutes() error {
	for _, cidr := range w.cfg.Routes {
		_, dstNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid route CIDR %s: %v", cidr, err)
		}
		route := netlink.Route{
			LinkIndex: w.link.Attrs().Index,
			Dst:       dstNet,
		}
		if err := netlink.RouteAdd(&route); err != nil && !isExistErr(err) {
			return fmt.Errorf("failed to add route %s: %v", cidr, err)
		}
	}
	return nil
}

// RemoveRoutes deletes the previously added routes
func (w *WireGuard) removeRoutes() error {
	for _, cidr := range w.cfg.Routes {
		_, dstNet, _ := net.ParseCIDR(cidr)
		route := netlink.Route{
			LinkIndex: w.link.Attrs().Index,
			Dst:       dstNet,
		}
		_ = netlink.RouteDel(&route) // best effort
	}
	log.Debug("routes removed", "interface", w.cfg.InterfaceName)
	return nil
}

// Teardown deletes the interface entirely (called on leadership loss)
func (w *WireGuard) teardown() error {
	if err := w.removeRoutes(); err != nil {
		return fmt.Errorf("failed to remove routes: %v", err)
	}
	if w.link != nil {
		if err := netlink.LinkDel(w.link); err != nil {
			return fmt.Errorf("failed to delete wg interface: %v", err)
		}
		w.link = nil
	}
	log.Debug("teared down complete", "interface", w.cfg.InterfaceName)
	return nil
}

// helper: check if error indicates interface/route exists
func isExistErr(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "file exists" || err.Error() == "Link exists"
}
