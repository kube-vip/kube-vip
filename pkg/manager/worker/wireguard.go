package worker

import (
	"context"
	log "log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Wireguard struct {
	Common
}

func NewWireguard(arpMgr *arp.Manager, intfMgr *networkinterface.Manager,
	config *kubevip.Config, closing *atomic.Bool, signalChan chan os.Signal,
	svcProcessor *services.Processor, mutex *sync.Mutex, clientSet *kubernetes.Clientset) *Wireguard {
	return &Wireguard{
		Common: Common{
			arpMgr:       arpMgr,
			intfMgr:      intfMgr,
			config:       config,
			closing:      closing,
			signalChan:   signalChan,
			svcProcessor: svcProcessor,
			mutex:        mutex,
			clientSet:    clientSet,
		},
	}
}

func (w *Wireguard) Configure(ctx context.Context) error {
	log.Info("reading wireguard peer configuration from Kubernetes secret")
	s, err := w.clientSet.CoreV1().Secrets(w.config.Namespace).Get(ctx, "wireguard", metav1.GetOptions{})
	if err != nil {
		return err
	}
	// parse all the details needed for Wireguard
	peerPublicKey := s.Data["peerPublicKey"]
	peerEndpoint := s.Data["peerEndpoint"]
	privateKey := s.Data["privateKey"]

	// Configure the interface to join the Wireguard VPN
	err = wireguard.ConfigureInterface(string(privateKey), string(peerPublicKey), string(peerEndpoint))
	if err != nil {
		return err
	}

	return nil
}

func (w *Wireguard) InitControlPlane() error {
	// NOT IMPLEMENTED
	return nil
}

func (w *Wireguard) StartControlPlane(ctx context.Context, clusterManager *cluster.Manager) {
	// NOT IMPLEMENTED
}

func (w *Wireguard) ConfigureServices() {
	// No configuration required
}

func (w *Wireguard) StartServices(ctx context.Context, id string) error {
	// Start a services watcher (all kube-vip pods will watch services), upon a new service
	// a lock based upon that service is created that they will all leaderElection on
	if w.config.EnableServicesElection {
		if err := w.ServicesPerServiceLeader(ctx); err != nil {
			return err
		}
	} else {
		w.ServicesGlobalLeader(ctx, id)
	}
	return nil
}

func (w *Wireguard) Name() string {
	return "Wireguard"
}
