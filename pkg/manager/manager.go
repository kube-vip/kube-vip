package manager

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/trafficmirror"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const plunderLock = "plndr-svcs-lock"

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet   *kubernetes.Clientset
	rwClientSet *kubernetes.Clientset
	CtrlClient  client.Client
	configMap   string
	config      *kubevip.Config

	// Manager services
	// service bool

	// Keeps track of all running instances
	serviceInstances []*Instance

	// UPNP functionality
	upnp bool
	// BGP Manager, this is a singleton that manages all BGP advertisements
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
func New(configMap string, config *kubevip.Config, ctrlClientOpt *client.Options) (*Manager, error) {

	// Instance identity should be the same as k8s node name to ensure better compatibility.
	// By default k8s sets node name to `hostname -s`,
	// so if node name is not provided in the config,
	// we set it to hostname as a fallback.
	// This mimics legacy behavior and should work on old kube-vip installations.
	if config.NodeName == "" {
		log.Warning("Node name is missing from the config, fall back to hostname")
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("could not get hostname: %v", err)
		}
		config.NodeName = hostname
	}
	log.Infof("Using node name [%v]", config.NodeName)

	adminConfigPath := "/etc/kubernetes/admin.conf"
	homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	var clientset *kubernetes.Clientset
	var clientConfig *rest.Config
	var err error

	switch {
	case config.LeaderElectionType == "etcd":
		// Do nothing, we don't construct a k8s client for etcd leader election
	case utils.FileExists(adminConfigPath):
		if config.KubernetesAddr != "" {
			log.Infof("k8s address [%s]", config.KubernetesAddr)
			clientConfig, err = k8s.NewRestConfig(adminConfigPath, false, config.KubernetesAddr)
		} else if config.EnableControlPlane {
			// If this is a control plane host it will likely have started as a static pod or won't have the
			// VIP up before trying to connect to the API server, we set the API endpoint to this machine to
			// ensure connectivity.
			if config.DetectControlPlane {
				clientConfig, err = k8s.FindWorkingKubernetesAddress(adminConfigPath, false)
			} else {
				// This will attempt to use kubernetes as the hostname (this should be passed as a host alias) in the pod manifest
				clientConfig, err = k8s.NewRestConfig(adminConfigPath, false, fmt.Sprintf("kubernetes:%v", config.Port))
			}
		} else {
			clientConfig, err = k8s.NewRestConfig(adminConfigPath, false, "")
		}
		if err != nil {
			return nil, fmt.Errorf("could not create k8s REST config from external file: %q: %w", adminConfigPath, err)
		}
		if clientset, err = k8s.NewClientset(clientConfig); err != nil {
			return nil, fmt.Errorf("could not create k8s clientset: %w", err)
		}
		log.Debugf("Using external Kubernetes configuration from file: %q", adminConfigPath)
	case utils.FileExists(homeConfigPath):
		clientConfig, err = k8s.NewRestConfig(homeConfigPath, false, "")
		if err != nil {
			return nil, fmt.Errorf("could not create k8s REST config from external file: %q: %w", homeConfigPath, err)
		}
		clientset, err = k8s.NewClientset(clientConfig)
		if err != nil {
			return nil, fmt.Errorf("could not create k8s clientset from external file: %q: %w", homeConfigPath, err)
		}
		log.Debugf("Using external Kubernetes configuration from file [%s]", homeConfigPath)
	default:
		clientConfig, err = k8s.NewRestConfig("", true, "")
		if err != nil {
			return nil, fmt.Errorf("could not create k8s REST config from incluster file: %q: %w", homeConfigPath, err)
		}
		clientset, err = k8s.NewClientset(clientConfig)
		if err != nil {
			return nil, fmt.Errorf("could not create k8s clientset from incluster config: %w", err)
		}
		log.Debug("Using external Kubernetes configuration from incluster config.")
	}

	var rwClientSet *kubernetes.Clientset
	var ctrlClient client.Client
	// if clientConfig is not nil, then we are not using etcd leader election
	// we need to create non-timeout clientset for RetryWatcher
	if clientConfig != nil {
		rwConfig := *clientConfig
		rwConfig.Timeout = 0
		rwClientSet, err = k8s.NewClientset(&rwConfig)
		if err != nil {
			return nil, fmt.Errorf("could not create k8s clientset for retry watcher: %w", err)
		}

		if ctrlClient, err = client.New(clientConfig, *ctrlClientOpt); err != nil {
			return nil, fmt.Errorf("could not create controller-runtime client: %w", err)
		}
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
		clientSet:   clientset,
		rwClientSet: rwClientSet,
		CtrlClient:  ctrlClient,
		configMap:   configMap,
		config:      config,
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

	if sm.config.EnableARP || sm.config.EnableWireguard {
		// Before starting the leader Election enable any additional functionality
		upnpEnabled, _ := strconv.ParseBool(os.Getenv("enableUPNP"))

		if upnpEnabled {
			sm.upnp = true
			clients := upnp.GetConnectionClients(context.TODO())
			if len(clients) == 0 {
				log.Errorf("Error Enabling UPNP. No Clients found")
				// Set the struct to false so nothing should use it in future
				sm.upnp = false
			} else {
				for _, c := range clients {
					ip, err := c.GetExternalIPAddress()
					log.Infof("Found UPNP IGD2 Gateway address[%s] error: [%s]", ip, err)
				}
			}
		}
		// TODO: It would be nice to run the UPNP refresh only on the leader.
		go sm.refreshUPNPForwards()
	}

	// If ARP is enabled then we start a LeaderElection that will use ARP to advertise VIPs
	if sm.config.EnableARP {
		log.Infoln("Starting Kube-vip Manager with the ARP engine")
		return sm.startARP(sm.config.NodeName)
	}

	if sm.config.EnableWireguard {
		log.Infoln("Starting Kube-vip Manager with the Wireguard engine")
		return sm.startWireguard(sm.config.NodeName)
	}

	if sm.config.EnableRoutingTable {
		log.Infoln("Starting Kube-vip Manager with the Routing Table engine")
		return sm.startTableMode(sm.config.NodeName)
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

func (sm *Manager) serviceInterface() string {
	svcIf := sm.config.Interface
	if sm.config.ServicesInterface != "" {
		svcIf = sm.config.ServicesInterface
	}
	return svcIf
}

func (sm *Manager) startTrafficMirroringIfEnabled() error {
	if sm.config.MirrorDestInterface != "" {
		svcIf := sm.serviceInterface()
		log.Infof("mirroring traffic from interface %s to interface %s", svcIf, sm.config.MirrorDestInterface)
		if err := trafficmirror.MirrorTrafficFromNIC(svcIf, sm.config.MirrorDestInterface); err != nil {
			return err
		}
	} else {
		log.Debug("skip starting traffic mirroring since it's not enabled.")
	}
	return nil
}

func (sm *Manager) stopTrafficMirroringIfEnabled() error {
	if sm.config.MirrorDestInterface != "" {
		svcIf := sm.serviceInterface()
		log.Infof("clean up qdisc config on interface %s", svcIf)
		if err := trafficmirror.CleanupQDSICFromNIC(svcIf); err != nil {
			return err
		}
	} else {
		log.Debug("skip stopping traffic mirroring since it's not enabled.")
	}
	return nil
}

func (sm *Manager) findServiceInstance(svc *v1.Service) *Instance {
	log.Debugf("service UID: %s", svc.UID)
	for i := range sm.serviceInstances {
		log.Debugf("saved service instance %d UID: %s", i, sm.serviceInstances[i].serviceSnapshot.UID)
		if sm.serviceInstances[i].serviceSnapshot.UID == svc.UID {
			return sm.serviceInstances[i]
		}
	}
	return nil
}

// Refresh UPNP Port Forwards for all Service Instances registered in the SM
func (sm *Manager) refreshUPNPForwards() {
	log.Info("Starting UPNP Port Refresher")
	for {
		time.Sleep(300 * time.Second)

		log.Infof("[UPNP] Refreshing %d Instances", len(sm.serviceInstances))
		for i := range sm.serviceInstances {
			sm.upnpMap(context.TODO(), sm.serviceInstances[i])
			if err := sm.updateStatus(sm.serviceInstances[i]); err != nil {
				log.Warnf("[UPNP] Error updating service IPs %s [%s]", sm.serviceInstances[i].serviceSnapshot.Name, err.Error())
			}
		}
	}
}
