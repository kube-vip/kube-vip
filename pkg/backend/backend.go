package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kube-vip/kube-vip/pkg/utils"
)

type BackendType string

const (
	Discovery BackendType = "discovery"
	HTTP      BackendType = "http"
)

type Backend interface {
	Check(context.Context) bool
	Address() string
	Port() uint16
	IsSameFamily(string) bool
	IsLocal() bool
}

func New(config *Config) (Backend, error) {
	switch config.Type {
	case Discovery:
		return newDiscoveryBackend(config), nil
	case HTTP:
		return kubernetesAddrBackend(config), nil
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
	KeepAddress    bool
	IsLocal        bool
}

type generic struct {
	addr           string
	port           uint16
	kubeConfigPath string
	isLocal        bool
}

func (g *generic) Address() string {
	return g.addr
}

func (g *generic) Port() uint16 {
	return g.port
}

func (g *generic) IsSameFamily(addr string) bool {
	a, err := url.Parse(g.addr)
	if err != nil {
		return false
	}
	if a.Hostname() != "" {
		return true
	}

	return utils.IsIPv6(addr) == utils.IsIPv6(g.addr)
}

func (g *generic) IsLocal() bool {
	return g.isLocal
}

type Map map[Backend]bool

func (m *Map) Find(addr string, port uint16) Backend {
	for b := range *m {
		if b.Address() == addr && b.Port() == port {
			return b
		}
	}
	return nil
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

// kubernetesAddrBackend converts an explicitly configured Kubernetes
// API address override (config.KubernetesAddr, e.g. "https://127.0.0.1:6443"
// on static-pod deployments) into a backend health-check entry. Returns nil
// when no usable override is configured.
func kubernetesAddrBackend(config *Config) Backend {
	if config.Address == "" {
		return nil
	}

	if config.KeepAddress {
		config.Port = 0
		return newHTTPBackend(config)
	}

	u, err := url.Parse(config.Address)
	if err != nil || u.Hostname() == "" {
		return nil
	}

	if p := u.Port(); p != "" {
		if parsed, err := strconv.ParseUint(p, 10, 16); err == nil {
			config.Port = uint16(parsed)
		}
	}

	config.Address = u.Hostname()

	return newHTTPBackend(config)
}
