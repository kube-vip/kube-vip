package cluster

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/equinixmetal"
	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"

	"github.com/packethost/packngo"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	watchtools "k8s.io/client-go/tools/watch"
)

const plunderLock = "plndr-cp-lock"

// Manager degines the manager of the load-balancing services
type Manager struct {
	KubernetesClient *kubernetes.Clientset
	// This channel is used to signal a shutdown
	SignalChan chan os.Signal
}

// NewManager will create a new managing object
func NewManager(path string, inCluster bool, port int) (*Manager, error) {
	var hostname string

	// If inCluster is set then it will likely have started as a static pod or won't have the
	// VIP up before trying to connect to the API server, we set the API endpoint to this machine to
	// ensure connectivity. Else if the path passed is empty and not running in the cluster,
	// attempt to look for a kubeconfig in the default HOME dir.
	if inCluster {
		hostname = fmt.Sprintf("kubernetes:%v", port)
	} else if len(path) == 0 && !inCluster {
		path = filepath.Join(os.Getenv("HOME"), ".kube", "config")

		// We modify the config so that we can always speak to the correct host
		id, err := os.Hostname()
		if err != nil {
			return nil, err
		}

		hostname = fmt.Sprintf("%s:%v", id, port)
	}

	clientset, err := k8s.NewClientset(path, inCluster, hostname)
	if err != nil {
		return nil, fmt.Errorf("error creating a new k8s clientset: %v", err)
	}

	return &Manager{
		KubernetesClient: clientset,
	}, nil
}

// StartCluster - Begins a running instance of the Leader Election cluster
func (cluster *Cluster) StartCluster(c *kubevip.Config, sm *Manager, bgpServer *bgp.Server) error {

	id, err := os.Hostname()
	if err != nil {
		return err
	}

	log.Infof("Beginning cluster membership, namespace [%s], lock name [%s], id [%s]", c.Namespace, plunderLock, id)

	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      plunderLock,
			Namespace: c.Namespace,
		},
		Client: sm.KubernetesClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// use a Go context so we can tell the arp loop code when we
	// want to step down
	ctxArp, cancelArp := context.WithCancel(context.Background())
	defer cancelArp()

	// use a Go context so we can tell the dns loop code when we
	// want to step down
	ctxDNS, cancelDNS := context.WithCancel(context.Background())
	defer cancelDNS()

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	go func() {
		<-signalChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
		// Cancel the arp context, which will in turn stop any broadcasts
	}()

	// (attempt to) Remove the virtual IP, incase it already exists
	err = cluster.Network.DeleteIP()
	if err != nil {
		log.Errorf("could not delete virtualIP: %v", err)
	}

	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if bgpServer != nil {
			bgpServer.Close()
		}
	}()

	// If Packet is enabled then we can begin our preparation work
	var packetClient *packngo.Client
	if c.EnableMetal {
		if c.ProviderConfig != "" {
			key, project, err := equinixmetal.GetPacketConfig(c.ProviderConfig)
			if err != nil {
				log.Error(err)
			} else {
				// Set the environment variable with the key for the project
				os.Setenv("PACKET_AUTH_TOKEN", key)
				// Update the configuration with the project key
				c.MetalProjectID = project
			}
		}
		packetClient, err = packngo.NewClient()
		if err != nil {
			log.Error(err)
		}

		// We're using Packet with BGP, popuplate the Peer information from the API
		if c.EnableBGP {
			log.Infoln("Looking up the BGP configuration from packet")
			err = equinixmetal.BGPLookup(packetClient, c)
			if err != nil {
				log.Error(err)
			}
		}
	}

	if c.EnableBGP {
		// Lets start BGP
		log.Info("Starting the BGP server to advertise VIP routes to VGP peers")
		bgpServer, err = bgp.NewBGPServer(&c.BGPConfig)
		if err != nil {
			log.Error(err)
		}
	}

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
		LeaseDuration:   time.Duration(c.LeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(c.RenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(c.RetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// As we're leading lets start the vip service
				err = cluster.vipService(ctxArp, ctxDNS, c, sm, bgpServer, packetClient)
				if err != nil {
					log.Errorf("Error starting the VIP service on the leader [%s]", err)
				}

			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Info("This node is becoming a follower within the cluster")

				// Stop the dns context
				cancelDNS()
				// Stop the Arp context if it is running
				cancelArp()

				// Stop the BGP server
				if bgpServer != nil {
					err = bgpServer.Close()
					if err != nil {
						log.Warnf("%v", err)
					}
				}

				err = cluster.Network.DeleteIP()
				if err != nil {
					log.Warnf("%v", err)
				}

				log.Fatal("lost leadership, restarting kube-vip")
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				log.Infof("Node [%s] is assuming leadership of the cluster", identity)
			},
		},
	})

	return nil
}

func (sm *Manager) NodeWatcher(lb *loadbalancer.IPVSLoadBalancer, port int) error {
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	log.Infof("Kube-Vip is watching nodes for control-plane labels")

	listOptions := metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.KubernetesClient.CoreV1().Nodes().Watch(context.Background(), listOptions)
		},
	})
	if err != nil {
		return fmt.Errorf("error creating label watcher: %s", err.Error())
	}

	go func() {
		<-sm.SignalChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context
		rw.Stop()
	}()

	ch := rw.ResultChan()
	//defer rw.Stop()

	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes Node from Annotation watcher")
			}
			//Find the node IP address (this isn't foolproof)
			for x := range node.Status.Addresses {

				if node.Status.Addresses[x].Type == v1.NodeInternalIP {
					err = lb.AddBackend(node.Status.Addresses[x].Address, port)
					if err != nil {
						log.Errorf("Add IPVS backend [%v]", err)
					}
				}
			}
		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes Node from Annotation watcher")
			}

			//Find the node IP address (this isn't foolproof)
			for x := range node.Status.Addresses {

				if node.Status.Addresses[x].Type == v1.NodeInternalIP {
					err = lb.RemoveBackend(node.Status.Addresses[x].Address, port)
					if err != nil {
						log.Errorf("Del IPVS backend [%v]", err)
					}
				}
			}

			log.Infof("Node [%s] has been deleted", node.Name)

		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes Nodes")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Errorf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))

			}

			status := statusErr.ErrStatus
			log.Errorf("%v", status)
		default:
		}
	}

	log.Infoln("Exiting Annotations watcher")
	return nil

}
