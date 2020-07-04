package cluster

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	"github.com/plunder-app/kube-vip/pkg/loadbalancer"
	"github.com/plunder-app/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const plunderLock = "plunder-lock"
const namespace = "kube-system"

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet *kubernetes.Clientset
}

// NewManager will create a new managing object
func NewManager(path string, inCluster bool) (*Manager, error) {
	var clientset *kubernetes.Clientset
	if inCluster {
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
		if path == "" {
			path = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		config, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			panic(err.Error())
		}

		// We modify the config so that we can always speak to the correct host
		id, err := os.Hostname()
		if err != nil {
			return nil, err
		}

		// TODO - we need to make the host/port configurable

		config.Host = fmt.Sprintf("%s:6443", id)
		clientset, err = kubernetes.NewForConfig(config)

		if err != nil {
			return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
		}
	}

	return &Manager{
		clientSet: clientset,
	}, nil
}

// StartLeaderCluster - Begins a running instance of the Raft cluster
func (cluster *Cluster) StartLeaderCluster(c *kubevip.Config, sm *Manager) error {

	id, err := os.Hostname()
	if err != nil {
		return err
	}
	log.Infof("Beginning cluster membership, namespace [%s], lock name [%s], id [%s]", namespace, plunderLock, id)

	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      plunderLock,
			Namespace: namespace,
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
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	// Add Notification for SIGKILL (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGKILL)

	go func() {
		<-signalChan
		log.Info("Received termination, signaling shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	// (attempt to) Remove the virtual IP, incase it already exists
	cluster.network.DeleteIP()

	// Managers for Vip load balancers and none-vip loadbalancers
	nonVipLB := loadbalancer.LBManager{}
	VipLB := loadbalancer.LBManager{}

	// Iterate through all Configurations
	if len(c.LoadBalancers) != 0 {
		for x := range c.LoadBalancers {
			// If the load balancer doesn't bind to the VIP
			if c.LoadBalancers[x].BindToVip == false {
				err = nonVipLB.Add("", &c.LoadBalancers[x])
				if err != nil {
					log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
				}
			}
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
		LeaseDuration:   5 * time.Second,
		RenewDeadline:   3 * time.Second,
		RetryPeriod:     1 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// we're notified when we start
				log.Info("This node is assuming leadership of the cluster")
				err = cluster.network.AddIP()
				if err != nil {
					log.Warnf("%v", err)
				}

				// Once we have the VIP running, start the load balancer(s) that bind to the VIP

				for x := range c.LoadBalancers {

					if c.LoadBalancers[x].BindToVip == true {
						err = VipLB.Add(c.VIP, &c.LoadBalancers[x])
						if err != nil {
							log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)

							// Stop all load balancers associated with the VIP
							err = VipLB.StopAll()
							if err != nil {
								log.Warnf("%v", err)
							}

							err = cluster.network.DeleteIP()
							if err != nil {
								log.Warnf("%v", err)
							}
						}
					}
				}

				if c.GratuitousARP == true {
					// Gratuitous ARP, will broadcast to new MAC <-> IP
					err = vip.ARPSendGratuitous(c.VIP, c.Interface)
					if err != nil {
						log.Warnf("%v", err)
					}
				}
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Infof("leader lost: %s", id)
				log.Info("This node is becoming a follower within the cluster")

				// Stop all load balancers associated with the VIP
				err = VipLB.StopAll()
				if err != nil {
					log.Warnf("%v", err)
				}

				err = cluster.network.DeleteIP()
				if err != nil {
					log.Warnf("%v", err)
				}

			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == id {
					result, err := cluster.network.IsSet()
					if err != nil {
						log.WithFields(log.Fields{"error": err, "ip": cluster.network.IP(), "interface": cluster.network.Interface()}).Error("Could not check ip")
					}

					if result == false {
						log.Error("This node is leader and is adopting the virtual IP")

						err = cluster.network.AddIP()
						if err != nil {
							log.Warnf("%v", err)
						}
						// Once we have the VIP running, start the load balancer(s) that bind to the VIP

						for x := range c.LoadBalancers {

							if c.LoadBalancers[x].BindToVip == true {
								err = VipLB.Add(c.VIP, &c.LoadBalancers[x])
								if err != nil {
									log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)
								}
							}
						}
						if c.GratuitousARP == true {
							// Gratuitous ARP, will broadcast to new MAC <-> IP
							err = vip.ARPSendGratuitous(c.VIP, c.Interface)
							if err != nil {
								log.Warnf("%v", err)
							}
						}
					}
				}
				log.Infof("new leader elected: %s", identity)
			},
		},
	})

	//<-signalChan
	log.Infof("Shutting down Kube-Vip Leader Election cluster")

	return nil
}
