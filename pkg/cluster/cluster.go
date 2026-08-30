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
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop                  chan struct{}
	stopMu                sync.Mutex
	workers               *workerGeneration
	Network               []vip.Network
	arpMgr                *arp.Manager
	routeMgr              *route.Manager
	nodeLabelMgr          node.Labeler
	labelAdded            bool
	healthCheckHTTPClient *http.Client
}

type workerGeneration struct {
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	preserve map[string]struct{}
	stopOnce sync.Once
	doneOnce sync.Once
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
		stop:                  make(chan struct{}),
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
			c.LoadBalancerForwardingMethod, c.IptablesBackend, c.EnableLoadBalancer, c.LoadBalancerPort,
			c.EnableServiceSecurity, intfMgr, c.EgressWithNftables, c.SkipDAD)
		if err != nil {
			return nil, err
		}
		networks = append(networks, network...)
	}

	return networks, nil
}

// Stop - Will stop the Cluster and release VIP if needed
func (cluster *Cluster) Stop() {
	cluster.stopWorkers(nil)
}

// StopWorkersAndWaitPreserving stops one exact worker generation and preserves
// only the addresses still owned by another Service.
func (cluster *Cluster) StopWorkersAndWaitPreserving(preserve map[string]struct{}) {
	done := cluster.stopWorkers(preserve)
	if done != nil {
		<-done
	}
}

// PrepareStopPreserving records addresses that must survive if this generation
// is stopped later. It deliberately does not close the generation stop channel.
func (cluster *Cluster) PrepareStopPreserving(preserve map[string]struct{}) {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	generation := cluster.workers
	if generation != nil {
		generation.setPreserve(preserve)
	}
}

func (cluster *Cluster) stopWorkers(preserve map[string]struct{}) <-chan struct{} {
	cluster.stopMu.Lock()
	generation := cluster.workers
	if generation == nil {
		stop := cluster.stop
		cluster.stop = make(chan struct{})
		cluster.stopMu.Unlock()
		close(stop)
		return nil
	}
	generation.setPreserve(preserve)
	stop := generation.stop
	done := generation.done
	cluster.stop = make(chan struct{})
	cluster.stopMu.Unlock()
	generation.stopOnce.Do(func() { close(stop) })
	return done
}

// WorkersRunning reports whether this cluster has a published generation that
// has not completed cleanup.
func (cluster *Cluster) WorkersRunning() bool {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	if cluster.workers == nil {
		return false
	}
	select {
	case <-cluster.workers.done:
		return false
	default:
		return true
	}
}

func (cluster *Cluster) finishWorkers(generation *workerGeneration) {
	generation.doneOnce.Do(func() {
		cluster.stopMu.Lock()
		if cluster.workers == generation {
			cluster.workers = nil
		}
		cluster.stopMu.Unlock()
		close(generation.done)
	})
}

func (generation *workerGeneration) setPreserve(preserve map[string]struct{}) {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	if generation.preserve == nil {
		generation.preserve = make(map[string]struct{}, len(preserve))
	}
	for address := range preserve {
		generation.preserve[address] = struct{}{}
	}
}

func (generation *workerGeneration) preservedAddresses() map[string]struct{} {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	preserve := make(map[string]struct{}, len(generation.preserve))
	for address := range generation.preserve {
		preserve[address] = struct{}{}
	}
	return preserve
}

func (cluster *Cluster) StopChannel() <-chan struct{} {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	return cluster.stop
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
	for _, network := range cluster.Network {
		cluster.cleanupVIP(c, network)
	}
}

func (cluster *Cluster) cleanupVIP(c *kubevip.Config, network vip.Network) {
	if c.EnableARP && cluster.arpMgr.Count(network.ARPName()) > 1 {
		return
	}

	if c.PreserveVIPOnLeadershipLoss {
		if utils.IsIPv6(network.IP()) {
			log.Info("[VIP] Removing IPv6 VIP immediately (required to prevent DAD failures on new leader)", "ip", network.IP())
			deleted, err := network.DeleteIP()
			if err != nil {
				log.Warn(err.Error())
			}
			if deleted {
				log.Info("deleted address", "IP", network.IP(), "interface", network.Interface())
			}
		} else {
			log.Info("[VIP] Preserving IPv4 VIP address on interface, only stopped ARP broadcasting", "ip", network.IP())
		}
		return
	}

	log.Info("[VIP] Deleting VIP", "ip", network.IP())
	deleted, err := network.DeleteIP()
	if err != nil {
		log.Warn(err.Error())
	}
	if deleted {
		log.Info("deleted address", "IP", network.IP(), "interface", network.Interface())
	}
}
