package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop                  chan bool
	once                  sync.Once
	Network               []vip.Network
	arpMgr                *arp.Manager
	healthCheckHTTPClient *http.Client
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool, intfMgr *networkinterface.Manager, arpMgr *arp.Manager) (*Cluster, error) {
	var networks []vip.Network
	var healthCheckHTTPClient *http.Client
	var err error

	if !disableVIP {
		// Start the Virtual IP Networking configuration
		networks, err = startNetworking(c, intfMgr)
		if err != nil {
			return nil, err
		}
	}

	if c.ControlPlaneHealthCheck.Address != "" {
		healthCheckHTTPClient, err = newHealthCheckHTTPClient(c)
		if err != nil {
			return nil, fmt.Errorf("initializing BGP health check client: %w", err)
		}
	}

	// Initialise the Cluster structure
	newCluster := &Cluster{
		Network:               networks,
		arpMgr:                arpMgr,
		stop:                  make(chan bool),
		healthCheckHTTPClient: healthCheckHTTPClient,
	}

	log.Debug("service security", "enabled", c.EnableServiceSecurity)

	return newCluster, nil
}

func startNetworking(c *kubevip.Config, intfMgr *networkinterface.Manager) ([]vip.Network, error) {
	address := c.VIP

	if c.Address != "" {
		address = c.Address
	}

	addresses := vip.Split(address)

	networks := []vip.Network{}
	for _, addr := range addresses {
		network, err := vip.NewConfig(addr, c.Interface, c.LoInterfaceGlobalScope, c.VIPSubnet, c.DDNS, c.DHCPMode,
			c.RequireDualStack, c.IsDualStack, c.RoutingTableID, c.RoutingTableType, c.RoutingProtocol, c.DNSMode,
			c.LoadBalancerForwardingMethod, c.IptablesBackend, c.EnableLoadBalancer, c.EnableServiceSecurity, intfMgr)
		if err != nil {
			return nil, err
		}
		networks = append(networks, network...)
	}

	return networks, nil
}

// Stop - Will stop the Cluster and release VIP if needed
func (cluster *Cluster) Stop() {
	// Close the stop channel, which will shut down the VIP (if needed)
	if cluster.stop != nil {
		cluster.once.Do(func() { // Ensure that the close channel can only ever be called once
			close(cluster.stop)
		})
	}
}

func newHealthCheckHTTPClient(c *kubevip.Config) (*http.Client, error) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected default HTTP transport type %T", http.DefaultTransport)
	}

	transport := defaultTransport.Clone()
	if c.ControlPlaneHealthCheck.CAPath != "" {
		caCert, err := os.ReadFile(c.ControlPlaneHealthCheck.CAPath)
		if err != nil {
			return nil, fmt.Errorf("reading health check CA cert %q: %w", c.ControlPlaneHealthCheck.CAPath, err)
		}

		rootCAs, err := x509.SystemCertPool()
		if err != nil || rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		if !rootCAs.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("health check CA cert %q contains no valid certificates", c.ControlPlaneHealthCheck.CAPath)
		}

		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
		}
		tlsConfig.RootCAs = rootCAs
		transport.TLSClientConfig = tlsConfig
	}

	return &http.Client{
		Timeout:   time.Duration(c.ControlPlaneHealthCheck.TimeoutSeconds) * time.Second,
		Transport: transport,
	}, nil
}
