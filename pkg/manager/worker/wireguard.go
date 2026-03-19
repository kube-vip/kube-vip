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
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
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
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type WireGuard struct {
	Common
	tunnelMgr           *wireguard.TunnelManager
	kubeAPIHost         string
	kubeAPIPort         string
	endpointWatcherCtx  context.Context
	endpointWatcherStop context.CancelFunc
	endpointWatcherWg   sync.WaitGroup
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

	// Start endpoint watcher - DNAT rules will be applied when endpoints arrive
	w.endpointWatcherCtx, w.endpointWatcherStop = context.WithCancel(ctx)
	w.endpointWatcherWg.Go(func() {
		w.watchKubernetesEndpoints(w.endpointWatcherCtx, tunnelConfig)
	})

	if w.config.EnableServices && !w.config.EnableServicesElection {
		if err := w.svcProcessor.ServicesWatcher(ctx, w.svcProcessor.SyncServices); err != nil {
			log.Error("failed to start services watcher", "err", err)
		}
	}
}

// watchKubernetesEndpoints watches the kubernetes service EndpointSlices for changes
// and updates the DNAT rules when API server endpoints change (e.g., when an API server goes down)
func (w *WireGuard) watchKubernetesEndpoints(ctx context.Context, tunnelConfig *wireguard.TunnelConfig) {
	log.Info("starting kubernetes endpoint watcher for control plane")

	kubeSvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubernetes",
			Namespace: "default",
		},
	}

	provider := providers.NewEndpointslices()
	rw, err := provider.CreateRetryWatcher(ctx, w.clientSet, kubeSvc)
	if err != nil {
		log.Error("failed to create kubernetes endpoint watcher", "err", err)
		return
	}
	defer rw.Stop()

	for event := range rw.ResultChan() {
		select {
		case <-ctx.Done():
			log.Info("kubernetes endpoint watcher stopped")
			return
		default:
		}

		switch event.Type {
		case watch.Added, watch.Modified, watch.Deleted:
			if err := provider.LoadObject(event.Object, func() {}); err != nil {
				log.Error("failed to load endpoint object", "err", err)
				continue
			}
			endpoints, _ := provider.GetAllEndpoints()
			log.Info("kubernetes endpoints changed, updating DNAT rules", "eventType", event.Type, "endpoints", endpoints)
			if err := w.updateControlPlaneDNAT(tunnelConfig, endpoints); err != nil {
				log.Error("failed to update control plane DNAT rules", "err", err)
			}
		case watch.Error:
			log.Warn("kubernetes endpoint watch error", "event", event)
		}
	}
}

// updateControlPlaneDNAT updates the DNAT rules for the control plane with the given endpoints
func (w *WireGuard) updateControlPlaneDNAT(tunnelConfig *wireguard.TunnelConfig, endpoints []string) error {
	if len(endpoints) == 0 {
		log.Warn("no kubernetes API server endpoints available")
		// Don't delete rules - keep routing to last known endpoints
		return nil
	}

	// Build targets with default port 6443
	targets := make([]nftables.DNATTarget, len(endpoints))
	for i, ep := range endpoints {
		targets[i] = nftables.DNATTarget{IP: ep, Port: 6443}
	}

	vipIP := utils.StripCIDR(w.config.VIP)

	err := nftables.ApplyDNAT(
		tunnelConfig.InterfaceName,
		vipIP,
		6443,
		targets,
		"controlplane",
		v1.ProtocolTCP,
		false,
		tunnelConfig.ListenPort,
	)
	if err != nil {
		return fmt.Errorf("failed to apply updated DNAT rule: %w", err)
	}

	log.Info("control plane DNAT rules updated", "targetCount", len(targets))
	return nil
}

func (w *WireGuard) OnStoppedLeading() {
	// we can do cleanup here
	w.mutex.Lock()
	defer w.mutex.Unlock()
	log.Info("leader lost", "id", w.config.NodeName)

	// Stop the kubernetes endpoint watcher and wait for it to finish
	if w.endpointWatcherStop != nil {
		w.endpointWatcherStop()
		w.endpointWatcherWg.Wait()
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
