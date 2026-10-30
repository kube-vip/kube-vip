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

	"github.com/kube-vip/kube-vip/pkg/utils"
)

type BackendType string

const (
	Discovery BackendType = "discovery"
	HTTP      BackendType = "http"
)

type AddressFamily uint8

const (
	AddressFamilyUnknown AddressFamily = iota
	AddressFamilyIPv4
	AddressFamilyIPv6
	AddressFamilyHostname
)

type Backend interface {
	Check(context.Context) bool
	Address() string
	Port() uint16
	Family() AddressFamily
	Key() Key
	IsLocal() bool
}

func New(config *Config) (Backend, error) {
	if err := processConfig(config); err != nil {
		return nil, fmt.Errorf("process backend config: %w", err)
	}
	switch config.Type {
	case Discovery:
		return newDiscoveryBackend(config), nil
	case HTTP:
		return newHTTPBackend(config)
	default:
		return nil, fmt.Errorf("backend of type %q is currently not supported", config.Type)
	}
}

type Config struct {
	Type           BackendType
	Address        string
	Port           uint16
	KubeConfigPath string
	Client         *http.Client
	IsLocal        bool
}

type generic struct {
	backendType    BackendType
	addr           string
	port           uint16
	family         AddressFamily
	kubeConfigPath string
	isLocal        bool
}

type addr struct {
	host string
	port uint16
}

func (a *addr) Address() string {
	out := ""
	out = a.host
	if utils.IsIPv6(a.host) {
		out = fmt.Sprintf("[%s]", a.host)
	}

	return out
}

func (g *generic) Address() string {
	return g.addr
}

func (g *generic) Port() uint16 {
	return g.port
}

func (g *generic) Family() AddressFamily {
	return g.family
}

func (g *generic) Key() Key {
	return NewKey(g.backendType, g.addr, g.port)
}

func addressFamily(address string) AddressFamily {
	host := address
	if parsed, err := url.Parse(address); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}

	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if ip.Is4() {
			return AddressFamilyIPv4
		}
		return AddressFamilyIPv6
	}

	if host != "" {
		return AddressFamilyHostname
	}
	return AddressFamilyUnknown
}

func (g *generic) IsLocal() bool {
	return g.isLocal
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
}

type Map map[Key]*Entry

func (m Map) Add(backend Backend, healthy bool) {
	m[backend.Key()] = &Entry{Backend: backend, Healthy: healthy}
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

// processConfig converts an explicitly configured Kubernetes
// API address override (config.KubernetesAddr, e.g. "https://127.0.0.1:6443"
// on static-pod deployments) into a backend health-check entry compatible config. Returns nil
// when no usable override is configured.
func processConfig(config *Config) error {
	if config.Address == "" {
		return fmt.Errorf("failed to create backend: empty address")
	}

	if config.Type == HTTP {
		if _, err := url.Parse(config.Address); err != nil {
			return fmt.Errorf("failed to parse address %q: %w", config.Address, err)
		}
		config.Port = 0
		return nil
	}

	a, err := processAddress(config.Address)
	if err != nil {
		return fmt.Errorf("failed to process Kubernetes address backend: %w", err)
	}

	if a.port != 0 {
		config.Port = a.port
	}

	config.Address = a.Address()

	return nil
}

func processAddress(address string) (*addr, error) {
	a := &addr{}

	adressTrimmed := strings.Trim(address, "[]")

	if utils.IsIPv4(adressTrimmed) || utils.IsIPv6(adressTrimmed) {
		a.host = adressTrimmed
		return a, nil
	}

	if ap, err := netip.ParseAddrPort(address); err == nil {
		a.host = ap.Addr().String()
		a.port = ap.Port()
		return a, nil
	}

	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("error parsing URL: %w", err)
	}

	if u.Hostname() != "" {
		a.host = u.Hostname()
	} else if u.Path != "" {
		a.host = u.Path
	}

	if a.host == "" {
		host, port, found := strings.Cut(address, ":")
		if !found {
			return nil, fmt.Errorf("invalid format for address %q: no port separator present", address)
		}
		if host == "" {
			return nil, fmt.Errorf("empty hostname for address %q", address)
		}
		a.host = host
		value, err := parsePort(port)
		if err != nil {
			return nil, fmt.Errorf("failed to parse port for address %q: %w", address, err)
		}
		a.port = value
		return a, nil
	}

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
