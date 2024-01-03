package manager

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/kube-vip/kube-vip/pkg/k8s"

	"github.com/kamhlos/upnp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
)

const plunderLock = "plndr-svcs-lock"

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet *kubernetes.Clientset
	configMap string
	config    *kubevip.Config

	// Manager services
	// service bool

	// Keeps track of all running instances
	serviceInstances []*Instance

	// Additional functionality
	upnp *upnp.Upnp

	//BGP Manager, this is a singleton that manages all BGP advertisements
	bgpServer *bgp.Server

	// This channel is used to catch an OS signal and trigger a shutdown
	signalChan chan os.Signal

	// This channel is used to signal a shutdown
	shutdownChan chan struct{}

	// This is a prometheus counter used to count the number of events received
	// from the service watcher
	countServiceWatchEvent *prometheus.CounterVec

	// This is a prometheus gauge indicating the state of the sessions.
	// 1 means "ESTABLISHED", 0 means "NOT ESTABLISHED"
	bgpSessionInfoGauge *prometheus.GaugeVec

	// This mutex is to protect calls from various goroutines
	mutex sync.Mutex
}

// New will create a new managing object
func New(configMap string, config *kubevip.Config) (*Manager, error) {

	var clientset *kubernetes.Clientset
	var err error

	adminConfigPath := "/etc/kubernetes/admin.conf"
	homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	switch {
	case config.LeaderElectionType == "etcd":
		// Do nothing, we don't construct a k8s client for etcd leader election
	case fileExists(adminConfigPath):
		if config.EnableControlPlane {
			// If this is a control plane host it will likely have started as a static pod or won't have the
			// VIP up before trying to connect to the API server, we set the API endpoint to this machine to
			// ensure connectivity.
			if config.DetectControlPlane {
				clientset, err = k8s.FindWorkingKubernetesAddress(adminConfigPath, false)
			} else {
				// This will attempt to use kubernetes as the hostname (this should be passed as a host alias) in the pod manifest
				clientset, err = k8s.NewClientset(adminConfigPath, false, fmt.Sprintf("kubernetes:%v", config.Port))
			}
		} else {
			clientset, err = k8s.NewClientset(adminConfigPath, false, "")
		}
		if err != nil {
			return nil, fmt.Errorf("could not create k8s clientset from external file: %q: %v", adminConfigPath, err)
		}
		log.Debugf("Using external Kubernetes configuration from file [%s]", adminConfigPath)
	case fileExists(homeConfigPath):
		clientset, err = k8s.NewClientset(homeConfigPath, false, "")
		if err != nil {
			return nil, fmt.Errorf("could not create k8s clientset from external file: %q: %v", homeConfigPath, err)
		}
		log.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)
	default:
		clientset, err = k8s.NewClientset("", true, "")
		if err != nil {
			return nil, fmt.Errorf("could not create k8s clientset from incluster config: %v", err)
		}
		log.Debug("Using external Kubernetes configuration from incluster config.")
	}

	// Flip this to something else
	// if config.DetectControlPlane {
	// 	log.Info("[k8s client] flipping to internal service account")
	// 	_, err = clientset.CoreV1().ServiceAccounts("kube-system").Apply(context.TODO(), kubevip.GenerateSA(), v1.ApplyOptions{FieldManager: "application/apply-patch"})
	// 	if err != nil {
	// 		return nil, fmt.Errorf("could not create k8s clientset from incluster config: %v", err)
	// 	}
	// 	_, err = clientset.RbacV1().ClusterRoles().Apply(context.TODO(), kubevip.GenerateCR(), v1.ApplyOptions{FieldManager: "application/apply-patch"})
	// 	if err != nil {
	// 		return nil, fmt.Errorf("could not create k8s clientset from incluster config: %v", err)
	// 	}
	// 	_, err = clientset.RbacV1().ClusterRoleBindings().Apply(context.TODO(), kubevip.GenerateCRB(), v1.ApplyOptions{FieldManager: "application/apply-patch"})
	// 	if err != nil {
	// 		return nil, fmt.Errorf("could not create k8s clientset from incluster config: %v", err)
	// 	}
	// }

	return &Manager{
		clientSet: clientset,
		configMap: configMap,
		config:    config,
		countServiceWatchEvent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "all_services_events",
			Help:      "Count all events fired by the service watcher categorised by event type",
		}, []string{"type"}),
		bgpSessionInfoGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "bgp_session_info",
			Help:      "Display state of session by setting metric for label value with current state to 1",
		}, []string{"state", "peer"}),
	}, nil
}

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) Start() error {

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	sm.signalChan = make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(sm.signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(sm.signalChan, syscall.SIGTERM)

	// All watchers and other goroutines should have an additional goroutine that blocks on this, to shut things down
	sm.shutdownChan = make(chan struct{})

	// If BGP is enabled then we start a server instance that will broadcast VIPs
	if sm.config.EnableBGP {

		// If Annotations have been set then we will look them up
		err := sm.parseAnnotations()
		if err != nil {
			return err
		}

		log.Infoln("Starting Kube-vip Manager with the BGP engine")
		return sm.startBGP()
	}

	// If ARP is enabled then we start a LeaderElection that will use ARP to advertise VIPs
	if sm.config.EnableARP {
		log.Infoln("Starting Kube-vip Manager with the ARP engine")
		return sm.startARP()
	}

	if sm.config.EnableWireguard {
		log.Infoln("Starting Kube-vip Manager with the Wireguard engine")
		return sm.startWireguard()
	}

	if sm.config.EnableRoutingTable {
		log.Infoln("Starting Kube-vip Manager with the Routing Table engine")
		return sm.startTableMode()
	}

	log.Errorln("prematurely exiting Load-balancer as no modes [ARP/BGP/Wireguard] are enabled")
	return nil
}

func returnNameSpace() (string, error) {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("unable to find Namespace")
}

func (sm *Manager) parseAnnotations() error {
	if sm.config.Annotations == "" {
		log.Debugf("No Node annotations to parse")
		return nil
	}

	err := sm.annotationsWatcher()
	if err != nil {
		return err
	}
	return nil
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
