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
	Address       string
	AllowedIPs    []string // CIDRs allowed through tunnel
	ListenPort    int
	KeepAlive     time.Duration
}

type WireGuard struct {
	cfg       WGConfig
	linkIndex *int
	client    *wgctrl.Client
}

func NewWireGuard(cfg WGConfig) *WireGuard {
	return &WireGuard{
		cfg: cfg,
	}
}

// Up brings up the interface, configures peers, and sets routes
func (w *WireGuard) Up() error {
	log.Info("bringing up wireguard interface", "interface", w.cfg.InterfaceName)
	if err := w.createInterface(); err != nil {
		_ = w.teardown()
		return err
	}
	if err := w.configurePeer(); err != nil {
		_ = w.teardown()
		return err
	}
	if err := w.addRoutes(); err != nil {
		_ = w.teardown()
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

	addr, err := netlink.ParseAddr(w.cfg.Address)
	if err != nil {
		return fmt.Errorf("failed to parse address: %s, %v", w.cfg.Address, err)
	}

	err = netlink.AddrAdd(link, addr)
	if err != nil {
		return fmt.Errorf("could not add address to link: %s, %v", w.cfg.Address, err)
	}

	if err = netlink.LinkSetMTU(link, 1420); err != nil {
		return fmt.Errorf("failed to set mtu %w", err)
	}

	mask := uint32(0xffffffff)
	err = netlink.RuleAdd(&netlink.Rule{
		Table:  w.cfg.ListenPort,
		Mark:   uint32(w.cfg.ListenPort),
		Mask:   &mask,
		Invert: true,
		Family: netlink.FAMILY_V4,
		Goto:   -1,
	})
	if err != nil {
		return fmt.Errorf("could not add mark rule to link: %v", err)
	}

	err = netlink.RuleAdd(&netlink.Rule{
		Table:             0,
		SuppressPrefixlen: 0,
		Goto:              -1,
	})
	if err != nil {
		return fmt.Errorf("could not add rule suppress_prefixlength to main table %w", err)
	}

	w.linkIndex = &link.Attrs().Index
	log.Info("created link", "interface", w.cfg.InterfaceName)
	return nil
}

// ConfigurePeer sets the private key, peer, allowed IPs, endpoint, keepalive
func (w *WireGuard) configurePeer() error {
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("failed to open wgctrl client: %v", err)
	}

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
		return fmt.Errorf("failed to resolve UDP address: %v", err)
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
		FirewallMark: &w.cfg.ListenPort,
	}

	if err = client.ConfigureDevice(w.cfg.InterfaceName, conf); err != nil {
		return fmt.Errorf("failed to configure wireguard device: %v", err)
	}
	w.client = client

	log.Info("wireguard device configured", "interface", w.cfg.InterfaceName)
	return nil
}

// AddRoutes creates the routes for the VIP/public IPs via the wg interface
func (w *WireGuard) addRoutes() error {
	for _, cidr := range w.cfg.AllowedIPs {
		_, dstNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Error("invalid route CIDR, skipping", "cidr", cidr, "err", err)
			continue
		}
		route := netlink.Route{
			LinkIndex: *w.linkIndex,
			Table:     w.cfg.ListenPort,
			Dst:       dstNet,
		}
		log.Info("Added route", "dst", dstNet.String())
		if err := netlink.RouteAdd(&route); err != nil && !isExistErr(err) {
			return fmt.Errorf("failed to add route %s: %v", cidr, err)
		}
	}
	return nil
}

// RemoveRoutes deletes the previously added routes
func (w *WireGuard) removeRoutes() error {
	for _, cidr := range w.cfg.AllowedIPs {
		_, dstNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Error("invalid route CIDR, skipping", "cidr", cidr, "err", err)
			continue
		}
		if w.linkIndex == nil {
			continue
		}
		route := netlink.Route{
			LinkIndex: *w.linkIndex,
			Dst:       dstNet,
			Table:     w.cfg.ListenPort,
		}
		err = netlink.RouteDel(&route) // best effort
		if err != nil {
			log.Error("failed to remove route", "cidr", cidr, "err", err)
		}
	}
	log.Info("routes removed", "interface", w.cfg.InterfaceName)
	return nil
}

func (w *WireGuard) removeRules() error {
	netlink.RuleDel(&netlink.Rule{
		Table: w.cfg.ListenPort,
		Goto:  -1,
	})
	return nil
}

// Teardown deletes the interface entirely (called on leadership loss)
func (w *WireGuard) teardown() error {
	if w.client != nil {
		if err := w.client.Close(); err != nil {
			return fmt.Errorf("failed to close wgctrl client: %v", err)
		}
		w.client = nil
	}
	if err := w.removeRoutes(); err != nil {
		return fmt.Errorf("failed to remove routes: %v", err)
	}

	if link, err := netlink.LinkByName(w.cfg.InterfaceName); err == nil {
		if err = netlink.LinkDel(link); err != nil {
			log.Error("failed to delete link", "err", err)
		}
	}
	w.linkIndex = nil

	err := w.removeRules()
	if err != nil {
		log.Error("failed to remove rules", "err", err)
	}

	log.Info("tear down complete", "interface", w.cfg.InterfaceName)
	return nil
}

// helper: check if error indicates interface/route exists
func isExistErr(err error) bool {
	if err == nil {
		return false
	}
	return err.Error() == "file exists" || err.Error() == "Link exists"
}
