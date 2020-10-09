package cluster

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/plunder-app/kube-vip/pkg/bgp"
	"github.com/plunder-app/kube-vip/pkg/kubevip"
	leaderelection "github.com/plunder-app/kube-vip/pkg/leaderElection"
	"github.com/plunder-app/kube-vip/pkg/loadbalancer"
	"github.com/plunder-app/kube-vip/pkg/packet"

	"github.com/plunder-app/kube-vip/pkg/vip"

	"github.com/packethost/packngo"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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

	// use a Go context so we can tell the arp loop code when we
	// want to step down
	ctxArp, cancelArp := context.WithCancel(context.Background())
	defer cancelArp()

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
		// Cancel the arp context, which will in turn stop any broadcasts
	}()

	// (attempt to) Remove the virtual IP, incase it already exists
	cluster.Network.DeleteIP()

	// Managers for Vip load balancers and none-vip loadbalancers
	nonVipLB := loadbalancer.LBManager{}
	VipLB := loadbalancer.LBManager{}

	// BGP server
	var bgpServer *bgp.Server
	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if bgpServer != nil {
			bgpServer.Close()
		}
	}()

	// If Packet is enabled then we can begin our preperation work
	var packetClient *packngo.Client
	if c.EnablePacket {
		packetClient, err = packngo.NewClient()
		if err != nil {
			log.Error(err)
		}

		// We're using Packet with BGP, popuplate the Peer information from the API
		if c.EnableBGP {
			log.Infoln("Looking up the BGP configuration from packet")
			err = packet.BGPLookup(packetClient, c)
			if err != nil {
				log.Error(err)
			}
		}
	}

	if c.EnableBGP {
		// Lets start BGP
		log.Info("Starting the BGP server to adverise VIP routes to VGP peers")
		bgpServer, err = bgp.NewBGPServer(&c.BGPConfig)
		if err != nil {
			log.Error(err)
		}
	}

	if c.EnableLoadBalancer {

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

				// we're notified when we start
				log.Info("This node is starting with leadership of the cluster")
				err = cluster.Network.AddIP()
				if err != nil {
					log.Warnf("%v", err)
				}

				if c.EnablePacket {
					// We're not using Packet with BGP
					if !c.EnableBGP {
						// Attempt to attach the EIP in the standard manner
						log.Debugf("Attaching the Packet EIP through the API to this host")
						err = packet.AttachEIP(packetClient, c, id)
						if err != nil {
							log.Error(err)
						}
					}
				}

				if c.EnableBGP {
					// Lets advertise the VIP over BGP, the host needs to be passed using CIDR notation
					cidrVip := fmt.Sprintf("%s/%s", cluster.Network.IP(), c.VIPCIDR)
					log.Debugf("Attempting to advertise the address [%s] over BGP", cidrVip)

					err = bgpServer.AddHost(cidrVip)
					if err != nil {
						log.Error(err)
					}
				}

				if c.EnableLoadBalancer {
					// Once we have the VIP running, start the load balancer(s) that bind to the VIP
					for x := range c.LoadBalancers {

						if c.LoadBalancers[x].BindToVip == true {
							err = VipLB.Add(cluster.Network.IP(), &c.LoadBalancers[x])
							if err != nil {
								log.Warnf("Error creating loadbalancer [%s] type [%s] -> error [%s]", c.LoadBalancers[x].Name, c.LoadBalancers[x].Type, err)

								// Stop all load balancers associated with the VIP
								err = VipLB.StopAll()
								if err != nil {
									log.Warnf("%v", err)
								}

								err = cluster.Network.DeleteIP()
								if err != nil {
									log.Warnf("%v", err)
								}
							}
						}
					}
				}

				if c.GratuitousARP == true {
					ctxArp, cancelArp = context.WithCancel(context.Background())

					go func(ctx context.Context) {
						for {
							select {
							case <-ctx.Done(): // if cancel() execute
								return
							default:
								// Gratuitous ARP, will broadcast to new MAC <-> IP
								err = vip.ARPSendGratuitous(cluster.Network.IP(), c.Interface)
								if err != nil {
									log.Warnf("%v", err)
								}
							}
							time.Sleep(3 * time.Second)
						}
					}(ctxArp)
				}
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				log.Info("This node is becoming a follower within the cluster")

				// Stop the Arp context if it is running
				cancelArp()

				// Stop the BGP server
				if bgpServer != nil {
					err = bgpServer.Close()
					if err != nil {
						log.Warnf("%v", err)
					}
				}

				// Stop all load balancers associated with the VIP
				err = VipLB.StopAll()
				if err != nil {
					log.Warnf("%v", err)
				}

				err = cluster.Network.DeleteIP()
				if err != nil {
					log.Warnf("%v", err)
				}
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				log.Infof("Node [%s] is assuming leadership of the cluster", identity)

				if identity == id {
					// We have the lock
				}
			},
		},
	})

	//<-signalChan
	log.Infof("Shutting down Kube-Vip Leader Election cluster")

	// Force a removal of the VIP (ignore the error if we don't have it)
	cluster.Network.DeleteIP()

	return nil
}
