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
	"github.com/kube-vip/kube-vip/pkg/vip"
)

// Cluster - The Cluster object manages the state of the cluster for a particular node
type Cluster struct {
	stop                  chan struct{}
	stopMu                sync.Mutex
	service               *servicesWorker
	Network               []vip.Network
	arpMgr                *arp.Manager
	routeMgr              *route.Manager
	nodeLabelMgr          node.Labeler
	labelAdded            bool
	healthCheckHTTPClient *http.Client
}

type servicesWorker struct {
	stop         chan struct{}
	done         chan struct{}
	stopping     bool
	preserveVIPs map[string]struct{}
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
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	if cluster.service != nil {
		workers := cluster.service
		if workers.stopping {
			return
		}
		workers.stopping = true
		cluster.stop = make(chan struct{})
		close(workers.stop)
		return
	}
	stop := cluster.stop
	cluster.stop = make(chan struct{})
	close(stop)
}

// StopAndWait signals the current Service worker generation and waits until it
// has finished its datapath cleanup.
func (cluster *Cluster) StopAndWait() {
	cluster.stopAndWait(nil)
}

// StopAndWaitPreserving stops the current Service worker generation while
// preserving the supplied VIPs for another Service that shares the same lease.
func (cluster *Cluster) StopAndWaitPreserving(addresses ...string) {
	preserve := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		preserve[address] = struct{}{}
	}
	cluster.stopAndWait(preserve)
}

func (cluster *Cluster) stopAndWait(preserveVIPs map[string]struct{}) {
	workers, signal := cluster.prepareServiceStop(preserveVIPs)
	if workers == nil {
		return
	}
	if signal {
		close(workers.stop)
	}
	<-workers.done
}

func (cluster *Cluster) prepareServiceStop(preserveVIPs map[string]struct{}) (*servicesWorker, bool) {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	workers := cluster.service
	if workers == nil {
		return nil, false
	}
	if workers.stopping {
		workers.preserveVIPs = mergeVIPs(workers.preserveVIPs, preserveVIPs)
		return workers, false
	}
	workers.stopping = true
	workers.preserveVIPs = preserveVIPs
	cluster.stop = make(chan struct{})
	return workers, true
}

func (cluster *Cluster) startServicesWorker() (<-chan struct{}, chan struct{}, error) {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	if cluster.service != nil {
		return nil, nil, fmt.Errorf("load balancer workers already running")
	}
	workers := &servicesWorker{stop: cluster.stop, done: make(chan struct{})}
	cluster.service = workers
	return workers.stop, workers.done, nil
}

func (cluster *Cluster) preserveServiceVIP(done chan struct{}, address string) bool {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	if cluster.service == nil || cluster.service.done != done {
		return false
	}
	_, preserve := cluster.service.preserveVIPs[address]
	return preserve
}

func mergeVIPs(existing, addresses map[string]struct{}) map[string]struct{} {
	if len(addresses) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(map[string]struct{}, len(addresses))
	}
	for address := range addresses {
		existing[address] = struct{}{}
	}
	return existing
}

func (cluster *Cluster) finishServicesWorker(done chan struct{}) {
	cluster.stopMu.Lock()
	defer cluster.stopMu.Unlock()
	if cluster.service == nil || cluster.service.done != done {
		return
	}
	cluster.service = nil
	close(done)
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

func (cluster *Cluster) cleanupVIPs(c *kubevip.Config) {
	for i := range cluster.Network {
		cluster.cleanupVIP(c, cluster.Network[i])
	}
}

func (cluster *Cluster) cleanupServiceVIPs(c *kubevip.Config, done chan struct{}) {
	for i := range cluster.Network {
		if cluster.preserveServiceVIP(done, cluster.Network[i].IP()) {
			continue
		}
		cluster.cleanupVIP(c, cluster.Network[i])
	}
}

func (cluster *Cluster) cleanupVIP(c *kubevip.Config, network vip.Network) {
	// layer2Update already removed this instance's own claim before calling
	// here, so any remaining count belongs to another service sharing the VIP.
	if c.EnableARP && cluster.arpMgr.Count(network.ARPName()) > 0 {
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
