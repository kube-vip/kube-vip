package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/plunder-app/kube-vip/pkg/cluster"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	watchtools "k8s.io/client-go/tools/watch"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
)

const plunderLock = "plunder-lock"

// OutSideCluster allows the controller to be started using a local kubeConfig for testing
var OutSideCluster bool

// EnableArp - determines the use of ARP broadcasts
var EnableArp bool

// Interface - determines the interface that all Loadbalancers will bind too
var Interface string

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

	ns, err := returnNameSpace()
	if err != nil {
		return err
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", sm.configMap),
	}

	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      plunderLock,
			Namespace: ns,
		},
		Client: sm.clientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	leaderChan := make(chan os.Signal, 1)
	signal.Notify(leaderChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-leaderChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	// start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// IMPORTANT: you MUST ensure that any code you have that
		// is protected by the lease must terminate **before**
		// you call cancel. Otherwise, you could have a background
		// loop still running and another process could
		// get elected before your background loop finished, violating
		// the stated goal of the lease.
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// we're notified when we start

				// Watch function
				// Use a restartable watcher, as this should help in the event of etcd or timeout issues
				rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
					WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
						return sm.clientSet.CoreV1().ConfigMaps(ns).Watch(listOptions)
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
				// signalChan := make(chan os.Signal, 1)
				// signal.Notify(signalChan, os.Interrupt)
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
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Infof("leader lost: %s", id)
				for x := range sm.serviceInstances {
					sm.serviceInstances[x].cluster.Stop()
				}
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == id {
					// I just got the lock
					return
				}
				log.Infof("new leader elected: %s", identity)
			},
		},
	})

	// <-signalChan
	log.Infof("Shutting down Kube-Vip")

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
