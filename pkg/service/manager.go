package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/kamhlos/upnp"

	dhclient "github.com/digineo/go-dhclient"
	"github.com/plunder-app/kube-vip/pkg/bgp"
	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
)

const plunderLock = "plunder-lock"

// OutSideCluster allows the controller to be started using a local kubeConfig for testing
var OutSideCluster bool

var signalChan chan os.Signal

type plndrServices struct {
	Services []service `json:"services"`
}

type dhcpService struct {
	// dhcpClient (used DHCP for the vip)
	dhcpClient    *dhclient.Client
	dhcpInterface string
}

type serviceInstance struct {
	// Virtual IP / Load Balancer configuration
	vipConfig kubevip.Config
	// Kubernetes service mapping
	service service
	// cluster instance
	cluster cluster.Cluster

	// Custom settings
	dhcp *dhcpService
}

// TODO - call from a package (duplicated struct in the cloud-provider code)
type service struct {
	Vip  string `json:"vip"`
	Port int    `json:"port"`
	UID  string `json:"uid"`
	Type string `json:"type"`

	ServiceName string `json:"serviceName"`
}

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet *kubernetes.Clientset
	configMap string
	config    *kubevip.Config

	// Keeps track of all running instances
	serviceInstances []serviceInstance

	// Additional functionality
	upnp *upnp.Upnp
	//BGP Manager, this is a singleton that manages all BGP advertisements
	bgpServer *bgp.Server
}

// NewManager will create a new managing object
func NewManager(configMap string, config *kubevip.Config) (*Manager, error) {
	var clientset *kubernetes.Clientset
	if OutSideCluster == false {
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
		return sm.startBGP()
	}

	// If ARP is enabled then we start a LeaderElection that will use ARP to advertise VIPs
	if sm.config.EnableARP {
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

// The watcher will watch config maps for new services being created
func (sm *Manager) watcher(ctx context.Context, ns string) {
	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", sm.configMap),
	}

	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().ConfigMaps(ns).Watch(context.TODO(), listOptions)
		},
	})

	if err != nil {
		log.Errorf("error creating watcher: %s", err.Error())
		ctx.Done()
	}

	ch := rw.ResultChan()
	defer rw.Stop()
	log.Infof("Beginning watching Kubernetes configMap [%s]", sm.configMap)

	var svcs plndrServices

	go func() {
		for event := range ch {

			// We need to inspect the event and get ResourceVersion out of it
			switch event.Type {
			case watch.Added, watch.Modified:
				log.Debugf("ConfigMap [%s] has been Created or modified", sm.configMap)
				cm, ok := event.Object.(*v1.ConfigMap)
				if !ok {
					log.Errorf("Unable to parse ConfigMap from watcher")
					break
				}
				data := cm.Data["plndr-services"]
				json.Unmarshal([]byte(data), &svcs)
				log.Debugf("Found %d services defined in ConfigMap", len(svcs.Services))

				err = sm.syncServices(&svcs)
				if err != nil {
					log.Errorf("%v", err)
				}
			case watch.Deleted:
				log.Debugf("ConfigMap [%s] has been Deleted", sm.configMap)

			case watch.Bookmark:
				// Un-used
			case watch.Error:
				log.Infoln("err")

				// This round trip allows us to handle unstructured status
				errObject := apierrors.FromObject(event.Object)
				statusErr, ok := errObject.(*apierrors.StatusError)
				if !ok {
					log.Fatalf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
					// Retry unknown errors
					//return false, 0
				}

				status := statusErr.ErrStatus
				log.Errorf("%v", status)

			default:
			}
		}
	}()

	<-signalChan
}
