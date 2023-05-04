//nolint:unparam

package service

import (
	"fmt"
	"os"
	"strings"

	"github.com/kamhlos/upnp"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
)

const plunderLock = "plunder-lock"

var signalChan chan os.Signal

// Manager defines the manager of the load-balancing services
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

	// This is a prometheus gauge indicating the state of the sessions.
	// 1 means "ESTABLISHED", 0 means "NOT ESTABLISHED"
	bgpSessionInfoGauge *prometheus.GaugeVec
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
func NewManager(configMap string, config *kubevip.Config, clientset *kubernetes.Clientset) (*Manager, error) {
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
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("Unable to find Namespace")
}
