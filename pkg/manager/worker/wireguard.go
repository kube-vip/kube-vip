package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/sysctl"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

type WireGuard struct {
	Common
	tunnelMgr           *wireguard.TunnelManager
	kubeAPIHost         string
	kubeAPIPort         string
	endpointWatcherCtx  context.Context
	endpointWatcherStop context.CancelFunc
}

func NewWireGuard(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, killFUnc func(),
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	electionMgr *election.Manager, leaseMgr *lease.Manager) *WireGuard {
	return &WireGuard{
		Common: *newCommon(arpMgr, intfMgr, config, closing, killFUnc,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr),
	}
}

func (w *WireGuard) Configure(ctx context.Context, _ *sync.WaitGroup) error {
	log.Info("reading wireguard tunnel configurations from Kubernetes secret")
	tunnelMgr := wireguard.NewTunnelManager()

	err := tunnelMgr.LoadConfigurationsFromSecret(ctx, w.clientSet, w.config.Namespace, "wireguard")
	if err != nil {
		return fmt.Errorf("failed to load WireGuard tunnel configurations: %w", err)
	}

	// Clean up any stale resources from previous runs (crash recovery for hostNetwork: true)
	// Must be called AFTER loading configs so we know which interfaces/ports to clean
	if err := tunnelMgr.CleanupStaleResources(); err != nil {
		log.Warn("failed to cleanup stale resources", "err", err)
		// Continue anyway - the cleanup is best-effort
	}

	if _, err := sysctl.EnableProcSys("/proc/sys/net/ipv4/conf/all/src_valid_mark"); err != nil {
		return fmt.Errorf("net.ipv4.conf.all.src_valid_mark is disabled and could not be enabled %w", err)
	}
	if _, err := sysctl.EnableProcSys("/proc/sys/net/ipv4/conf/all/route_localnet"); err != nil {
		return fmt.Errorf("net.ipv4.conf.all.route_localnet is disabled and could not be enabled %w", err)
	}

	w.tunnelMgr = tunnelMgr
	configuredVIPs := tunnelMgr.ListConfiguredTunnels()
	log.Info("loaded WireGuard tunnel configurations", "vips", configuredVIPs)

	return nil
}

func (w *WireGuard) InitControlPlane() error {
	// Get Kubernetes service IP and port from environment
	w.kubeAPIHost = os.Getenv("KUBERNETES_SERVICE_HOST")
	w.kubeAPIPort = os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if w.kubeAPIHost == "" || w.kubeAPIPort == "" {
		return fmt.Errorf("KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT_HTTPS not set")
	}
	return nil
}

func (w *WireGuard) StartControlPlane(ctx context.Context, electionManager *election.Manager) {
	if !w.tunnelMgr.HasConfigForVIP(w.config.VIP) {
		log.Error("no WireGuard tunnel configuration found for control plane VIP", "vip", w.config.VIP)
		return
	}
	w.runGlobalElection(ctx, w, w.config.LeaseName, w.config, electionManager)
}

func (w *WireGuard) ConfigureServices() {
	w.svcProcessor.TunnelMgr = w.tunnelMgr
}

func (w *WireGuard) StartServices(ctx context.Context) error {
	if w.config.EnableServicesElection {
		log.Info("beginning watching services, leaderelection will happen for every service")
		err := w.svcProcessor.StartServicesWatchForLeaderElection(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *WireGuard) Name() string {
	return "WireGuard"
}

func (w *WireGuard) OnStartedLeading(ctx context.Context) {
	// Bring up the WireGuard tunnel for control plane VIP
	err := w.tunnelMgr.BringUpTunnelForVIP(w.config.VIP)
	if err != nil {
		log.Error("could not start wireguard tunnel for control plane", "vip", w.config.VIP, "err", err)
		_ = w.tunnelMgr.TearDownTunnelForVIP(w.config.VIP)
		w.killFunc()
		return
	}

	// Get the tunnel to access its configuration
	wg := w.tunnelMgr.GetTunnelForVIP(w.config.VIP)
	if wg == nil {
		log.Error("failed to get wireguard tunnel after bringing up", "vip", w.config.VIP)
		w.killFunc()
		return
	}

	tunnelConfig := w.tunnelMgr.GetConfigForVIP(w.config.VIP)
	if tunnelConfig == nil {
		log.Error("failed to get tunnel configuration", "vip", w.config.VIP)
		_ = w.tunnelMgr.TearDownTunnelForVIP(w.config.VIP)
		w.killFunc()
		return
	}

	// Strip CIDR notation from VIP if present
	vipIP := utils.StripCIDR(w.config.VIP)

	// Fetch the kubernetes API server endpoints from the "kubernetes" service in default namespace
	targets, err := w.fetchKubernetesEndpoints(ctx)
	if err != nil {
		log.Error("failed to fetch kubernetes endpoints", "err", err)
		_ = w.tunnelMgr.TearDownTunnelForVIP(w.config.VIP)
		w.killFunc()
		return
	}

	if len(targets) == 0 {
		log.Error("no kubernetes API server endpoints found")
		_ = w.tunnelMgr.TearDownTunnelForVIP(w.config.VIP)
		w.killFunc()
		return
	}

	// Apply nftables DNAT rule with load balancing across all API server endpoints
	log.Info("applying nftables DNAT rule with load balancing",
		"interface", tunnelConfig.InterfaceName,
		"vip", vipIP,
		"sourcePort", 6443,
		"targets", targets)

	// Control plane targets API server endpoints directly, needs masquerade (localEndpoint=false)
	err = nftables.ApplyDNAT(
		tunnelConfig.InterfaceName,
		vipIP,
		6443,
		targets,
		"controlplane",
		false,
		v1.ProtocolTCP,
		false,
		tunnelConfig.ListenPort,
	)
	if err != nil {
		log.Error("could not apply nftables DNAT rule", "err", err)
		_ = w.tunnelMgr.TearDownTunnelForVIP(w.config.VIP)
		w.killFunc()
		return
	}

	if w.config.EnableServices && !w.config.EnableServicesElection {
		if err := w.svcProcessor.ServicesWatcher(ctx, w.svcProcessor.SyncServices); err != nil {
			log.Error("failed to start services watcher", "err", err)
		}
	}
	log.Info("nftables DNAT rule applied successfully", "targetCount", len(targets))

	// Start endpoint watcher for kubernetes service to handle API server changes
	w.endpointWatcherCtx, w.endpointWatcherStop = context.WithCancel(ctx)
	go w.watchKubernetesEndpoints(w.endpointWatcherCtx, tunnelConfig)
}

// fetchKubernetesEndpoints fetches the endpoints for the "kubernetes" service in default namespace
func (w *WireGuard) fetchKubernetesEndpoints(ctx context.Context) ([]nftables.DNATTarget, error) {
	// List EndpointSlices for the kubernetes service
	endpointSlices, err := w.clientSet.DiscoveryV1().EndpointSlices("default").List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=kubernetes",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list kubernetes endpointslices: %w", err)
	}

	var targets []nftables.DNATTarget
	for _, eps := range endpointSlices.Items {
		// Get the port (should be 6443 or similar for HTTPS)
		var targetPort uint16
		for _, port := range eps.Ports {
			if port.Port != nil && (port.Name == nil || *port.Name == "https") {
				targetPort = uint16(*port.Port) //nolint:gosec // Port range validated by Kubernetes
				break
			}
		}
		if targetPort == 0 {
			// Default to 6443 if no port found
			targetPort = 6443
		}

		for _, endpoint := range eps.Endpoints {
			// Only use ready endpoints
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			for _, addr := range endpoint.Addresses {
				targets = append(targets, nftables.DNATTarget{
					IP:   addr,
					Port: targetPort,
				})
			}
		}
	}

	return targets, nil
}

// watchKubernetesEndpoints watches the kubernetes service EndpointSlices for changes
// and updates the DNAT rules when API server endpoints change (e.g., when an API server goes down)
func (w *WireGuard) watchKubernetesEndpoints(ctx context.Context, tunnelConfig *wireguard.TunnelConfig) {
	log.Info("starting kubernetes endpoint watcher for control plane")

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/service-name": "kubernetes"}}
	opts := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}

	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return w.clientSet.DiscoveryV1().EndpointSlices("default").Watch(ctx, opts)
		},
	})
	if err != nil {
		log.Error("failed to create kubernetes endpoint watcher", "err", err)
		return
	}
	defer rw.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("kubernetes endpoint watcher stopped")
			return
		case event, ok := <-rw.ResultChan():
			if !ok {
				log.Warn("kubernetes endpoint watcher channel closed, restarting")
				// Channel closed, try to restart the watcher
				rw, err = watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
					WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
						return w.clientSet.DiscoveryV1().EndpointSlices("default").Watch(ctx, opts)
					},
				})
				if err != nil {
					log.Error("failed to restart kubernetes endpoint watcher", "err", err)
					return
				}
				continue
			}

			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				log.Info("kubernetes endpoints changed, updating DNAT rules", "eventType", event.Type)
				if err := w.updateControlPlaneDNAT(ctx, tunnelConfig); err != nil {
					log.Error("failed to update control plane DNAT rules", "err", err)
				}
			case watch.Error:
				log.Warn("kubernetes endpoint watch error", "event", event)
			}
		}
	}
}

// updateControlPlaneDNAT updates the DNAT rules for the control plane with current kubernetes endpoints
func (w *WireGuard) updateControlPlaneDNAT(ctx context.Context, tunnelConfig *wireguard.TunnelConfig) error {
	targets, err := w.fetchKubernetesEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch kubernetes endpoints: %w", err)
	}

	if len(targets) == 0 {
		log.Warn("no kubernetes API server endpoints available")
		// Don't delete rules - keep routing to last known endpoints
		// This prevents complete API unavailability during brief transitions
		return nil
	}

	vipIP := utils.StripCIDR(w.config.VIP)

	log.Info("updating control plane DNAT rules",
		"interface", tunnelConfig.InterfaceName,
		"vip", vipIP,
		"targets", targets)

	// Delete existing rule and apply new one
	if err := nftables.DeleteDNATRule(tunnelConfig.InterfaceName, false, "controlplane"); err != nil {
		log.Warn("failed to delete existing control plane DNAT rule", "err", err)
		// Continue anyway - ApplyDNAT will overwrite
	}

	err = nftables.ApplyDNAT(
		tunnelConfig.InterfaceName,
		vipIP,
		6443,
		targets,
		"controlplane",
		false,
		v1.ProtocolTCP,
		false,
		tunnelConfig.ListenPort,
	)
	if err != nil {
		return fmt.Errorf("failed to apply updated DNAT rule: %w", err)
	}

	log.Info("control plane DNAT rules updated successfully", "targetCount", len(targets))
	return nil
}

func (w *WireGuard) OnStoppedLeading() {
	// we can do cleanup here
	w.mutex.Lock()
	defer w.mutex.Unlock()
	log.Info("leader lost", "id", w.config.NodeName)

	// Stop the kubernetes endpoint watcher
	if w.endpointWatcherStop != nil {
		w.endpointWatcherStop()
	}

	log.Info("deleting nftables DNAT chains")
	err := nftables.DeleteIngressChains(false, "controlplane")
	if err != nil {
		log.Error("could not delete DNAT ingress chains", "err", err)
	} else {
		log.Info("nftables DNAT chains deleted successfully")
	}

	// Tear down all tunnels (control plane + services)
	err = w.tunnelMgr.TearDownAllTunnels()
	if err != nil {
		log.Error("failed to tear down tunnels", "err", err)
	}
	if w.config.EnableServices && !w.config.EnableServicesElection {
		w.svcProcessor.Stop()
	}
	log.Error("lost control plane leadership, restarting kube-vip")
	w.killFunc()
}

func (w *WireGuard) OnNewLeader(identity string) {
	// we're notified when new leader elected
	if identity == w.config.NodeName {
		// I just got the lock
		return
	}
	// safety check - tear down tunnel if we're not the leader
	_ = w.tunnelMgr.TearDownTunnelForVIP(w.config.VIP)
	log.Info("new leader elected", "id", identity)
}
