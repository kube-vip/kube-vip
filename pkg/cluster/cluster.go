package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop                  chan bool
	Network               []vip.Network
	arpMgr                *arp.Manager
	routeMgr              *route.Manager
	nodeLabelMgr          node.Labeler
	labelAdded            bool
	healthCheckHTTPClient *http.Client
}

// InitCluster - Will attempt to initialise all of the required settings for the cluster
func InitCluster(c *kubevip.Config, disableVIP bool, intfMgr *networkinterface.Manager, arpMgr *arp.Manager,
	routeMgr *route.Manager, nodeLabelMgr node.Labeler) (*Cluster, error) {
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
		routeMgr:              routeMgr,
		nodeLabelMgr:          nodeLabelMgr,
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
		close(cluster.stop)
		cluster.stop = make(chan bool) // recreate channel for future use
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

// cleanupVIPs handles VIP removal based on the PreserveVIPOnLeadershipLoss configuration.
// When preservation is enabled, IPv6 VIPs are always removed immediately to prevent DAD
// failures on the new leader, while IPv4 VIPs are intentionally left in place.
// When preservation is disabled (legacy behavior), all VIPs are removed.
func (cluster *Cluster) cleanupVIPs(c *kubevip.Config) {
	for i := range cluster.Network {
		if c.EnableARP && cluster.arpMgr.Count(cluster.Network[i].ARPName()) > 1 {
			continue
		}

		if c.PreserveVIPOnLeadershipLoss {
			if utils.IsIPv6(cluster.Network[i].IP()) {
				log.Info("[VIP] Removing IPv6 VIP immediately (required to prevent DAD failures on new leader)", "ip", cluster.Network[i].IP())
				deleted, err := cluster.Network[i].DeleteIP()
				if err != nil {
					log.Warn(err.Error())
				}
				if deleted {
					log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
				}
			} else {
				log.Info("[VIP] Preserving IPv4 VIP address on interface, only stopped ARP broadcasting", "ip", cluster.Network[i].IP())
			}
		} else {
			log.Info("[VIP] Deleting VIP", "ip", cluster.Network[i].IP())
			deleted, err := cluster.Network[i].DeleteIP()
			if err != nil {
				log.Warn(err.Error())
			}
			if deleted {
				log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
			}
		}
	}
}
