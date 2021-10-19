package service

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamhlos/upnp"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

const plunderLock = "plunder-lock"

// OutSideCluster allows the controller to be started using a local kubeConfig for testing
var OutSideCluster bool

var signalChan chan os.Signal

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet *kubernetes.Clientset
	configMap string
	config    *kubevip.Config

	// Keeps track of all running instances
	serviceInstances []Instance

	// Additional functionality
	upnp *upnp.Upnp

	//BGP Manager, this is a singleton that manages all BGP advertisements
	bgpServer *bgp.Server

	// This channel is used to signal a shutdown
	signalChan chan os.Signal

	// This is a prometheus counter used to count the number of events received
	// from the service watcher
	countServiceWatchEvent *prometheus.CounterVec
}

// Instance defines an instance of everything needed to manage a vip
type Instance struct {
	// Virtual IP / Load Balancer configuration
	vipConfig kubevip.Config

	// cluster instance
	cluster cluster.Cluster

	// Service uses DHCP
	isDHCP              bool
	dhcpInterface       string
	dhcpInterfaceHwaddr string
	dhcpInterfaceIP     string
	dhcpClient          *vip.DHCPClient

	// Kubernetes service mapping
	Vip  string
	Port int32
	UID  string
	Type string

	ServiceName string
}

// NewManager will create a new managing object
func NewManager(configMap string, config *kubevip.Config) (*Manager, error) {
	var clientset *kubernetes.Clientset
	if !OutSideCluster {
		// This will attempt to load the configuration when running within a POD
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("error creating kubernetes client config: %s", err.Error())
		}
		clientset, err = kubernetes.NewForConfig(cfg)

		if err != nil {
			return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
		}
		// use the current context in kubeconfig
	} else {
		config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
		if err != nil {
			panic(err.Error())
		}
		clientset, err = kubernetes.NewForConfig(config)

		if err != nil {
			return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
		}
	}

	return &Manager{
		clientSet: clientset,
		configMap: configMap,
		config:    config,
	}, nil
}

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) Start() error {

	// If BGP is enabled then we start a server instance that will broadcast VIPs
	if sm.config.EnableBGP {
		log.Infoln("Starting loadBalancer Service with the BGP engine")
		return sm.startBGP()
	}

	// If ARP is enabled then we start a LeaderElection that will use ARP to advertise VIPs
	if sm.config.EnableARP {
		log.Infoln("Starting loadBalancer Service with the ARP engine")
		return sm.startARP()
	}

	log.Infoln("Prematurely exiting Load-balancer as neither Layer2 or Layer3 is enabled")
	return nil
}

func returnNameSpace() (string, error) {
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("Unable to find Namespace")
}
