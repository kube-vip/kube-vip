package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type BackendType string

const (
	Discovery BackendType = "discovery"
	HTTP      BackendType = "http"
)

type Backend interface {
	Check(context.Context) (bool, error)
	Address() string
	Port() uint16
	Key() Key
}

func New(config *Config) (Backend, error) {
	cfg, err := processConfig(config)
	if err != nil {
		return nil, fmt.Errorf("process backend config: %w", err)
	}
	switch cfg.Type {
	case Discovery:
		return newDiscoveryBackend(cfg), nil
	case HTTP:
		return newHTTPBackend(cfg)
	default:
		return nil, fmt.Errorf("backend of type %q is currently not supported", cfg.Type)
	}
}

type Config struct {
	Type           BackendType
	Address        string
	Port           uint16
	KubeConfigPath string
	Client         *http.Client
}

type generic struct {
	backendType BackendType
	addr        string
	port        uint16
}

type addr struct {
	host string
	port uint16
}

func (g *generic) Address() string {
	return g.addr
}

func (g *generic) Port() uint16 {
	return g.port
}

func (g *generic) Key() Key {
	return NewKey(g.backendType, g.addr, g.port)
}

type Key struct {
	Type    BackendType
	Address string
	Port    uint16
}

func NewKey(backendType BackendType, address string, port uint16) Key {
	unwrappedAddress := address
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		unwrappedAddress = address[1 : len(address)-1]
	}
	if ip, err := netip.ParseAddr(unwrappedAddress); err == nil {
		address = ip.String()
	}
	return Key{Type: backendType, Address: address, Port: port}
}

type Entry struct {
	Backend Backend
	Healthy bool
	// Applied reports whether the backend destination is installed in IPVS.
	// Only used by the IPVS load balancer.
	Applied bool
	// IsLocal reports whether the backend destination is n local address.
	// Only used by the IPVS load balancer.
	IsLocal bool
}

type Map map[Key]*Entry

func (m Map) Add(entry *Entry) {
	m[entry.Backend.Key()] = entry
}

func (m Map) Find(backendType BackendType, address string, port uint16) *Entry {
	return m[NewKey(backendType, address, port)]
}

func Watch(ctx context.Context, interval int, tickAction func()) {
	if interval <= 0 {
		interval = 5
	}

	ticker := time.NewTicker(time.Second * time.Duration(interval))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickAction()
		}
	}
}

// processConfig validates config.Address and normalises it:
// HTTP addresses are kept as URLs (port ignored), other addresses are reduced to a
// bare host, with an embedded port overriding config.Port.
func processConfig(config *Config) (*Config, error) {
	cfg := *config
	if cfg.Address == "" {
		return nil, fmt.Errorf("failed to create backend: empty address")
	}

	if cfg.Type == HTTP {
		if _, err := url.Parse(cfg.Address); err != nil {
			return nil, fmt.Errorf("failed to parse address %q: %w", cfg.Address, err)
		}
		cfg.Port = 0
		return &cfg, nil
	}

	a, err := processAddress(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("failed to process Kubernetes address backend: %w", err)
	}

	if a.port != 0 {
		cfg.Port = a.port
	}

	cfg.Address = a.host
	return &cfg, nil
}

func processAddress(address string) (*addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(address, "[]")); err == nil {
		if ip.Zone() != "" {
			return nil, fmt.Errorf("zoned IP address %q is not supported", address)
		}
		return &addr{host: ip.String()}, nil
	}

	tmpAddr := address

	if !strings.Contains(tmpAddr, "://") {
		tmpAddr = "//" + tmpAddr
	}

	u, err := url.Parse(tmpAddr)
	if err != nil {
		return nil, fmt.Errorf("error parsing URL %q: %w", address, err)
	}

	if u.Hostname() == "" {
		return nil, fmt.Errorf("empty hostname for address %q", address)
	}

	a := &addr{host: u.Hostname()}
	if u.Port() != "" {
		value, err := parsePort(u.Port())
		if err != nil {
			return nil, fmt.Errorf("failed to parse port for address %q: %w", address, err)
		}
		a.port = value
	}

	return a, nil
}

func parsePort(portStr string) (uint16, error) {
	value, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("failed to parse port %q: %v", portStr, err)
	}
	return uint16(value), nil
}
