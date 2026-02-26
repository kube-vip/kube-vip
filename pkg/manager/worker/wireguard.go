package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type WireGuard struct {
	Common
	wg          *wireguard.WireGuard
	kubeAPIHost string
	kubeAPIPort string
}

func NewWireGuard(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset,
	electionMgr *election.Manager, leaseMgr *lease.Manager) *WireGuard {
	return &WireGuard{
		Common: *newCommon(arpMgr, intfMgr, config, closing, signalChan,
			svcProcessor, mutex, clientSet, electionMgr, leaseMgr),
	}
}

func (w *WireGuard) Configure(ctx context.Context, _ *sync.WaitGroup) error {
	log.Info("reading wireguard peer configuration from Kubernetes secret")
	s, err := w.clientSet.CoreV1().Secrets(w.config.Namespace).Get(ctx, "wireguard", metav1.GetOptions{})
	if err != nil {
		return err
	}
	// parse all the details needed for Wireguard
	peerPublicKey := string(s.Data["peerPublicKey"])
	peerEndpoint := string(s.Data["peerEndpoint"])
	privateKey := string(s.Data["privateKey"])
	allowedIPs := string(s.Data["allowedIPs"])
	listenPort := string(s.Data["listenPort"])
	if listenPort == "" {
		listenPort = "51820"
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil {
		return fmt.Errorf("failed to convert listenPort to integer: %w", err)
	}
	IPs := make([]string, 0)
	for ip := range strings.SplitSeq(allowedIPs, ",") {
		IPs = append(IPs, strings.TrimSpace(ip))
	}
	cfg := wireguard.WGConfig{
		PrivateKey:    privateKey,
		PeerPublicKey: peerPublicKey,
		PeerEndpoint:  peerEndpoint,
		InterfaceName: "wg0",
		Address:       w.config.VIP,
		KeepAlive:     time.Duration(5) * time.Second,
		AllowedIPs:    IPs,
		ListenPort:    port,
	}
	w.wg = wireguard.NewWireGuard(cfg)

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
	w.runGlobalElection(ctx, w, w.config.LeaseName, w.config, electionManager)
}

func (w *WireGuard) ConfigureServices() {
	// NOT IMPLEMENTED
}

func (w *WireGuard) StartServices(ctx context.Context) error {
	// NOT IMPLEMENTED
	return nil
}

func (w *WireGuard) Name() string {
	return "WireGuard"
}

func (w *WireGuard) OnStartedLeading(ctx context.Context) {
	log.Info("started leading", "id", w.config.NodeName)
	err := w.wg.Up()
	if err != nil {
		log.Error("could not start wireguard", "err", err)
		_ = w.wg.Down()
		if !w.closing.Load() {
			w.signalChan <- syscall.SIGINT
		}
	}

	// Strip CIDR notation from VIP if present
	vipIP := w.config.VIP
	if strings.Contains(vipIP, "/") {
		ip, _, err := net.ParseCIDR(vipIP)
		if err != nil {
			log.Error("could not parse VIP CIDR", "err", err, "vip", vipIP)
			_ = w.wg.Down()
			if !w.closing.Load() {
				w.signalChan <- syscall.SIGINT
			}
		}
		vipIP = ip.String()
	}

	// Parse Kubernetes API port
	kubeAPIPortInt, err := strconv.ParseUint(w.kubeAPIPort, 10, 16)
	if err != nil {
		log.Error("could not parse KUBERNETES_SERVICE_PORT_HTTPS", "err", err, "port", w.kubeAPIPort)
		_ = w.wg.Down()
		if !w.closing.Load() {
			w.signalChan <- syscall.SIGINT
		}
	}

	// Apply nftables DNAT rule to route traffic from wg0:6443 to Kubernetes API service
	log.Info("applying nftables DNAT rule", "interface", "wg0", "vip", vipIP, "sourcePort", 6443, "kubeAPIHost", w.kubeAPIHost, "kubeAPIPort", w.kubeAPIPort)
	err = nftables.ApplyAPIServerDNAT("wg0", vipIP, w.kubeAPIHost, 6443, uint16(kubeAPIPortInt), "controlplane", false)
	if err != nil {
		log.Error("could not apply nftables DNAT rule, restarting kube-vip", "err", err)
		_ = w.wg.Down()
		if !w.closing.Load() {
			w.signalChan <- syscall.SIGINT
		}
	}
	log.Info("nftables DNAT rule applied successfully")
}

func (w *WireGuard) OnStoppedLeading() {
	// we can do cleanup here
	w.mutex.Lock()
	defer w.mutex.Unlock()
	log.Info("leader lost", "id", w.config.NodeName)

	log.Info("deleting nftables DNAT chains")
	err := nftables.DeleteIngressChains(false, "controlplane")
	if err != nil {
		log.Error("could not delete DNAT ingress chains", "err", err)
	} else {
		log.Info("nftables DNAT chains deleted successfully")
	}

	err = w.wg.Down()
	if err != nil {
		log.Error(err.Error(), "id", w.config.NodeName)
	}

	log.Error("lost leadership, restarting kube-vip")
	if !w.closing.Load() {
		w.signalChan <- syscall.SIGINT
	}
}

func (w *WireGuard) OnNewLeader(identity string) {
	// we're notified when new leader elected
	if identity == w.config.NodeName {
		// I just got the lock
		return
	}
	// safety check
	_ = w.wg.Down()
	log.Info("new leader elected", "id", identity)
}
