package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/davecgh/go-spew/spew"
	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/cluster"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	watchtools "k8s.io/client-go/tools/watch"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// OutSideCluster allows the controller to be started using a local kubeConfig for testing
var OutSideCluster bool

type plndrServices struct {
	Services []service `json:"services"`
}

type serviceInstance struct {
	// Virtual IP / Load Balancer configuration
	vipConfig kubevip.Config
	// Kubernetes service mapping
	service service
	// cluster instance
	cluster cluster.Cluster
}

// TODO - call from a package (duplicated struct in the cloud-provider code)
type service struct {
	Vip         string `json:"vip"`
	Port        int    `json:"port"`
	UID         string `json:"uid"`
	ServiceName string `json:"serviceName"`
}

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet *kubernetes.Clientset
	configMap string
	// Keeps track of all running instances
	serviceInstances []serviceInstance
}

// NewManager will create a new managing object
func NewManager(configMap string) (*Manager, error) {
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
	}, nil
}

// Start will begin the ConfigMap watcher
func (sm *Manager) Start() error {
	// TODO - Needs changing to (with cancel)
	ctx := context.TODO()

	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", sm.configMap),
	}
	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().ConfigMaps("default").Watch(ctx, listOptions)
		},
	})

	if err != nil {
		return fmt.Errorf("error creating watcher: %s", err.Error())
	}

	ch := rw.ResultChan()
	defer rw.Stop()
	log.Infof("Beginning watching Kubernetes configMap [%s]", sm.configMap)

	var svcs plndrServices

	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			log.Debugf("ConfigMap [%s] has been Created or modified", sm.configMap)
			cm, ok := event.Object.(*v1.ConfigMap)
			if !ok {
				return fmt.Errorf("Unable to parse ConfigMap from watcher")
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

	return nil
}
