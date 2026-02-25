package wireguard

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TunnelConfig represents a single WireGuard tunnel configuration
type TunnelConfig struct {
	VIP           string   `yaml:"vip"`           // The VIP this tunnel serves (e.g., "10.0.0.100/24")
	PrivateKey    string   `yaml:"privateKey"`    // WireGuard private key
	PeerPublicKey string   `yaml:"peerPublicKey"` // Peer's public key
	PeerEndpoint  string   `yaml:"peerEndpoint"`  // Peer endpoint (IP:Port)
	AllowedIPs    []string `yaml:"allowedIPs"`    // Allowed IPs through tunnel
	ListenPort    int      `yaml:"listenPort"`    // Local listen port

	// Internal fields (not from YAML)
	Name          string // Unique name for this tunnel (e.g., "tunnel1")
	InterfaceName string // Interface name (e.g., "wg0", "wg1")
}

// TunnelManager manages multiple WireGuard tunnels
type TunnelManager struct {
	mu       sync.RWMutex
	tunnels  map[string]*WireGuard    // key: VIP (without CIDR), value: WireGuard instance
	configs  map[string]*TunnelConfig // key: VIP (without CIDR), value: TunnelConfig
	refCount map[string]int           // key: VIP (without CIDR), value: number of consumers using the tunnel
}

// NewTunnelManager creates a new tunnel manager
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels:  make(map[string]*WireGuard),
		configs:  make(map[string]*TunnelConfig),
		refCount: make(map[string]int),
	}
}

// LoadConfigurationsFromSecret loads WireGuard tunnel configurations from a Kubernetes secret
// Secret format (YAML string in tunnels key):
//
//	data:
//	  tunnels: |
//	    tunnel1:
//	      vip: 10.0.0.100/24
//	      privateKey: <key>
//	      peerPublicKey: <key>
//	      peerEndpoint: 203.0.113.1:51820
//	      allowedIPs:
//	        - 10.0.0.0/24
//	      listenPort: 51820
//	    tunnel2:
//	      vip: 10.0.0.101/24
//	      privateKey: <key>
//	      peerPublicKey: <key>
//	      peerEndpoint: 203.0.113.2:51821
//	      allowedIPs:
//	        - 10.0.0.0/24
//	      listenPort: 51821
func (tm *TunnelManager) LoadConfigurationsFromSecret(ctx context.Context, clientSet *kubernetes.Clientset, namespace, secretName string) error {
	secret, err := clientSet.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
	}

	tunnelsData, ok := secret.Data["tunnels"]
	if !ok {
		return fmt.Errorf("secret %s/%s must contain 'tunnels' key", namespace, secretName)
	}

	return tm.parseTunnelConfig(tunnelsData)
}

// parseTunnelConfig parses the YAML tunnel configuration
func (tm *TunnelManager) parseTunnelConfig(data []byte) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Parse YAML into a map of tunnel name -> TunnelConfig
	var tunnelsMap map[string]*TunnelConfig
	if err := yaml.Unmarshal(data, &tunnelsMap); err != nil {
		return fmt.Errorf("failed to parse tunnel configuration YAML: %w", err)
	}

	if len(tunnelsMap) == 0 {
		return fmt.Errorf("no tunnel configurations found in secret")
	}

	// Use the tunnel name from the YAML key as the interface name
	// This ensures deterministic interface naming across restarts
	for name, config := range tunnelsMap {
		config.Name = name
		config.InterfaceName = name

		if err := tm.addTunnelConfig(config); err != nil {
			return fmt.Errorf("invalid tunnel config %s: %w", name, err)
		}
	}

	log.Info("loaded tunnel configurations", "count", len(tm.configs))
	return nil
}

// addTunnelConfig adds a tunnel configuration to the manager
func (tm *TunnelManager) addTunnelConfig(config *TunnelConfig) error {
	// Validate required fields
	if config.VIP == "" {
		return fmt.Errorf("vip is required")
	}
	if config.PrivateKey == "" {
		return fmt.Errorf("privateKey is required")
	}
	if config.PeerPublicKey == "" {
		return fmt.Errorf("peerPublicKey is required")
	}
	if config.PeerEndpoint == "" {
		return fmt.Errorf("peerEndpoint is required")
	}
	if config.ListenPort < 1 || config.ListenPort > 65535 {
		return fmt.Errorf("listenPort must be between 1 and 65535")
	}

	// Extract VIP without CIDR for indexing
	vipKey := utils.StripCIDR(config.VIP)

	// Check for duplicate VIP
	if _, exists := tm.configs[vipKey]; exists {
		return fmt.Errorf("duplicate VIP configuration: %s", config.VIP)
	}

	tm.configs[vipKey] = config
	log.Info("added tunnel configuration",
		"name", config.Name,
		"vip", config.VIP,
		"interface", config.InterfaceName,
		"listenPort", config.ListenPort)

	return nil
}

// GetTunnelForVIP returns the WireGuard tunnel instance for a given VIP
// If the tunnel doesn't exist yet, it returns nil
func (tm *TunnelManager) GetTunnelForVIP(vip string) *WireGuard {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	vipKey := utils.StripCIDR(vip)
	return tm.tunnels[vipKey]
}

// GetConfigForVIP returns the tunnel configuration for a given VIP
func (tm *TunnelManager) GetConfigForVIP(vip string) *TunnelConfig {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	vipKey := utils.StripCIDR(vip)
	return tm.configs[vipKey]
}

// BringUpTunnelForVIP creates and brings up a WireGuard tunnel for the given VIP.
// If the tunnel is already up, it increments the reference count.
// Multiple consumers (control plane, services) can share the same VIP tunnel.
func (tm *TunnelManager) BringUpTunnelForVIP(vip string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	vipKey := utils.StripCIDR(vip)

	// Check if already up - increment reference count
	if _, exists := tm.tunnels[vipKey]; exists {
		tm.refCount[vipKey]++
		log.Debug("tunnel already up, incremented reference count", "vip", vip, "refCount", tm.refCount[vipKey])
		return nil
	}

	// Get configuration
	config, exists := tm.configs[vipKey]
	if !exists {
		return fmt.Errorf("no tunnel configuration found for VIP %s", vip)
	}

	// Create WireGuard configuration
	wgCfg := WGConfig{
		InterfaceName: config.InterfaceName,
		PrivateKey:    config.PrivateKey,
		PeerPublicKey: config.PeerPublicKey,
		PeerEndpoint:  config.PeerEndpoint,
		Address:       config.VIP,
		AllowedIPs:    config.AllowedIPs,
		ListenPort:    config.ListenPort,
		KeepAlive:     5 * time.Second,
	}

	// Create and bring up the tunnel
	wg := NewWireGuard(wgCfg)
	if err := wg.Up(); err != nil {
		return fmt.Errorf("failed to bring up tunnel for VIP %s: %w", vip, err)
	}

	tm.tunnels[vipKey] = wg
	tm.refCount[vipKey] = 1
	log.Info("brought up WireGuard tunnel",
		"vip", vip,
		"interface", config.InterfaceName,
		"listenPort", config.ListenPort)

	return nil
}

// TearDownTunnelForVIP decrements the reference count for the given VIP tunnel.
// The tunnel is only torn down when the reference count reaches zero.
// This allows multiple consumers (control plane, services) to share the same VIP tunnel.
func (tm *TunnelManager) TearDownTunnelForVIP(vip string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	vipKey := utils.StripCIDR(vip)

	wg, exists := tm.tunnels[vipKey]
	if !exists {
		log.Debug("tunnel not found for teardown", "vip", vip)
		return nil
	}

	// Decrement reference count
	tm.refCount[vipKey]--
	if tm.refCount[vipKey] > 0 {
		log.Debug("tunnel still in use, decremented reference count", "vip", vip, "refCount", tm.refCount[vipKey])
		return nil
	}

	// Reference count is zero, tear down the tunnel
	if err := wg.Down(); err != nil {
		log.Error("failed to tear down tunnel", "vip", vip, "err", err)
		// Continue to remove from map even if teardown failed
	}

	delete(tm.tunnels, vipKey)
	delete(tm.refCount, vipKey)
	log.Info("tore down WireGuard tunnel", "vip", vip)

	return nil
}

// TearDownAllTunnels tears down all active tunnels regardless of reference count.
// This is typically called during shutdown.
func (tm *TunnelManager) TearDownAllTunnels() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	var errors []error
	for vip, wg := range tm.tunnels {
		if err := wg.Down(); err != nil {
			log.Error("failed to tear down tunnel", "vip", vip, "err", err)
			errors = append(errors, err)
		}
	}

	tm.tunnels = make(map[string]*WireGuard)
	tm.refCount = make(map[string]int)
	log.Info("tore down all WireGuard tunnels")

	if len(errors) > 0 {
		return fmt.Errorf("errors occurred during teardown: %v", errors)
	}

	return nil
}

// GetRefCount returns the current reference count for a VIP tunnel.
// Returns 0 if the tunnel doesn't exist.
func (tm *TunnelManager) GetRefCount(vip string) int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	vipKey := utils.StripCIDR(vip)
	return tm.refCount[vipKey]
}

// ListActiveTunnels returns a list of VIPs with active tunnels
func (tm *TunnelManager) ListActiveTunnels() []string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	vips := make([]string, 0, len(tm.tunnels))
	for vip := range tm.tunnels {
		vips = append(vips, vip)
	}

	return vips
}

// ListConfiguredTunnels returns a list of VIPs with configurations
func (tm *TunnelManager) ListConfiguredTunnels() []string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	vips := make([]string, 0, len(tm.configs))
	for vip := range tm.configs {
		vips = append(vips, vip)
	}

	return vips
}

// HasConfigForVIP checks if a tunnel configuration exists for the given VIP
func (tm *TunnelManager) HasConfigForVIP(vip string) bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	vipKey := utils.StripCIDR(vip)
	_, exists := tm.configs[vipKey]
	return exists
}
