package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/etcd"
	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
	"github.com/kube-vip/kube-vip/pkg/utils"

	log "log/slog"

	clientv3 "go.etcd.io/etcd/client/v3"
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

// Manager degines the manager of the load-balancing services
type Manager struct {
	KubernetesClient   *kubernetes.Clientset
	RetryWatcherClient *kubernetes.Clientset
	// This channel is used to signal a shutdown
	SignalChan chan os.Signal

	EtcdClient *clientv3.Client
}

// NewManager will create a new managing object
func NewManager(path string, inCluster bool, port int) (*Manager, error) {
	var hostname string

	// If inCluster is set then it will likely have started as a static pod or won't have the
	// VIP up before trying to connect to the API server, we set the API endpoint to this machine to
	// ensure connectivity. Else if the path passed is empty and not running in the cluster,
	// attempt to look for a kubeconfig in the default HOME dir.

	hostname = fmt.Sprintf("kubernetes:%v", port)

	if len(path) == 0 && !inCluster {
		path = filepath.Join(os.Getenv("HOME"), ".kube", "config")

		// We modify the config so that we can always speak to the correct host
		id, err := os.Hostname()
		if err != nil {
			return nil, err
		}

		hostname = fmt.Sprintf("%s:%v", id, port)
	}

	config, err := k8s.NewRestConfig(path, inCluster, hostname)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s REST config: %w", err)
	}

	clientset, err := k8s.NewClientset(config)
	if err != nil {
		return nil, fmt.Errorf("error creating a new k8s clientset: %v", err)
	}

	rwConfig, err := k8s.NewRestConfig(path, inCluster, hostname)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s REST config for retryClientSet: %w", err)
	}

	rwConfig.Timeout = 0 // empty value to disable the timeout
	rwClientSet, err := k8s.NewClientset(rwConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client for retry watcher: %w", err)
	}

	return &Manager{
		KubernetesClient:   clientset,
		RetryWatcherClient: rwClientSet,
	}, nil
}

// StartCluster - Begins a running instance of the Leader Election cluster
func (cluster *Cluster) StartCluster(ctx context.Context, c *kubevip.Config,
	sm *Manager, bgpServer *bgp.Server, leaseMgr *lease.Manager) error {
	var err error

	ns, leaseName := lease.NamespaceName(c.LeaseName, c)

	log.Info("cluster membership", "namespace", ns, "lock", leaseName, "id", c.NodeName)

	leaseID := fmt.Sprintf("%s/%s", ns, leaseName)
	objectName := fmt.Sprintf("%s-cp", leaseID)
	objLease, newLease, sharedLease := leaseMgr.Add(leaseID, objectName)

	if !newLease {
		log.Debug("this election was alreadty done, waiting for it to finish", "lease", leaseName)
		select {
		case <-ctx.Done():
		case <-objLease.Ctx.Done():
		}
		leaseMgr.Delete(leaseID, objectName)
		return nil
	}

	// Start a goroutine that will delete the lease when the service context is cancelled.
	// This is important for proper cleanup when a service is deleted - it ensures that
	// the lease context (svcLease.Ctx) gets cancelled, which causes RunOrDie to return.
	// Without this, RunOrDie would continue running until leadership is naturally lost.
	go func() {
		<-ctx.Done()
		leaseMgr.Delete(leaseID, objectName)
	}()

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	signalChan := make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(signalChan, syscall.SIGTERM)

	if cluster.completed == nil {
		cluster.completed = make(chan bool, 1)
		defer close(cluster.completed)
	}

	if cluster.stop == nil {
		cluster.stop = make(chan bool, 1)
	}

	go func() {
		select {
		case <-signalChan:
		case <-cluster.stop:
		}

		log.Info("Received termination, signaling cluster shutdown")
		objLease.Cancel()
	}()

	// (attempt to) Remove the virtual IP, in case it already exists

	for i := range cluster.Network {
		deleted, err := cluster.Network[i].DeleteIP()
		if err != nil {
			log.Error("could not delete virtualIP", "err", err)
		}
		if deleted {
			log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
		}
	}

	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if bgpServer != nil {
			bgpServer.Close()
		}
	}()

	if c.EnableBGP && bgpServer == nil {
		// Lets start BGP
		log.Info("Starting the BGP server to advertise VIP routes to VGP peers")
		bgpServer, err = bgp.NewBGPServer(c.BGPConfig)
		if err != nil {
			log.Error("new BGP server", "err", err)
		}
		if err := bgpServer.Start(ctx, nil); err != nil {
			log.Error("starting BGP server", "err", err)
		}
	}

	// this object is sharing lease with another object
	if sharedLease {
		log.Debug("this election was alreadty done, shared lease", "lease", leaseName)
		// wait for leader election to start or context to be done
		select {
		case <-objLease.Started:
		case <-objLease.Ctx.Done():
			// Lease was cancelled (e.g., leader election ended), return immediately
			// This allows the restart loop to create a fresh lease
			log.Debug("lease context cancelled before leader election started", "lease", leaseName)
			return fmt.Errorf("lease %q context cancelled before leader election started", leaseName)
		}

		// When we become leader, ensure we can take over VIPs even if they're preserved on other nodes
		if c.PreserveVIPOnLeadershipLoss {
			log.Info("Becoming leader with VIP preservation enabled - ensuring VIP takeover")
			// Force add the VIPs (this will work even if they exist due to the precheck logic)
			for i := range cluster.Network {
				added, err := cluster.Network[i].AddIP(true, false)
				if err != nil {
					log.Error("failed to ensure VIP on leader takeover", "vip", cluster.Network[i].IP(), "err", err)
				} else if added {
					log.Info("took over VIP as new leader", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
				} else {
					log.Info("VIP already configured on interface", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
				}
			}
		}

		// Start ARP advertisements now that we have leadership
		log.Info("Start ARP/NDP advertisement")
		go cluster.arpMgr.StartAdvertisement(ctx)

		// As we're leading lets start the vip service
		err := cluster.vipService(ctx, c, sm, bgpServer, objLease.Cancel)
		if err != nil {
			log.Error("starting VIP service on leader", "err", err)
			signalChan <- syscall.SIGINT
		}

		log.Debug("cluster waiting for leader context done", "lease", leaseName)
		// wait for leaderelection to be finished
		<-objLease.Ctx.Done()

		// Handle VIP cleanup based on configuration
		if c.PreserveVIPOnLeadershipLoss {
			// For IPv6, we must remove VIPs immediately to avoid DAD failures on the new leader
			// IPv6 Duplicate Address Detection will fail if the new leader tries to add an IP that is still present on this node's interface
			// We need to check each VIP individually and only remove IPv6 VIPs
			for i := range cluster.Network {
				if utils.IsIPv6(cluster.Network[i].IP()) {
					log.Info("Removing IPv6 VIP immediately (required to prevent DAD failures on new leader)", "ip", cluster.Network[i].IP())
					deleted, err := cluster.Network[i].DeleteIP()
					if err != nil {
						log.Warn("delete VIP", "err", err)
					}
					if deleted {
						log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					}
				} else {
					log.Info("Preserving IPv4 VIP address on interface, only stopped ARP broadcasting", "ip", cluster.Network[i].IP())
				}
			}
		} else {
			// Legacy behavior: delete VIP addresses on leadership loss
			log.Info("Deleting VIP addresses on leadership loss (legacy behavior)")
			for i := range cluster.Network {
				log.Info("Deleting VIP addresses on leadership loss (legacy behavior)", "i", i, "IP", cluster.Network[i].IP())
				deleted, err := cluster.Network[i].DeleteIP()
				if err != nil {
					log.Warn("delete VIP", "err", err)
				}
				if deleted {
					log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
				}
			}
		}

		return nil
	}

	run := &runConfig{
		config:    c,
		leaseID:   c.NodeName,
		leaseName: leaseName,
		namespace: ns,
		sm:        sm,
		onStartedLeading: func(context.Context) {
			close(objLease.Started)
			// When we become leader, ensure we can take over VIPs even if they're preserved on other nodes
			if c.PreserveVIPOnLeadershipLoss {
				log.Info("Becoming leader with VIP preservation enabled - ensuring VIP takeover")
				// Force add the VIPs (this will work even if they exist due to the precheck logic)
				for i := range cluster.Network {
					added, err := cluster.Network[i].AddIP(true, false)
					if err != nil {
						log.Error("failed to ensure VIP on leader takeover", "vip", cluster.Network[i].IP(), "err", err)
					} else if added {
						log.Info("took over VIP as new leader", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					} else {
						log.Info("VIP already configured on interface", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					}
				}
			}

			// Start ARP advertisements now that we have leadership
			log.Info("Start ARP/NDP advertisement")
			go cluster.arpMgr.StartAdvertisement(objLease.Ctx)

			// As we're leading lets start the vip service
			err := cluster.vipService(objLease.Ctx, c, sm, bgpServer, objLease.Cancel)
			if err != nil {
				log.Error("starting VIP service on leader", "err", err)
				signalChan <- syscall.SIGINT
			}
		},
		onStoppedLeading: func() {
			// we can do cleanup here
			log.Info("This node is becoming a follower within the cluster")

			// Stop the BGP server
			if bgpServer != nil {
				err := bgpServer.Close()
				if err != nil {
					log.Warn("close BGP server", "err", err)
				}
			}

			// Handle VIP cleanup based on configuration
			if c.PreserveVIPOnLeadershipLoss {
				// For IPv6, we must remove VIPs immediately to avoid DAD failures on the new leader
				// IPv6 Duplicate Address Detection will fail if the new leader tries to add an IP that is still present on this node's interface
				// We need to check each VIP individually and only remove IPv6 VIPs
				for i := range cluster.Network {
					if utils.IsIPv6(cluster.Network[i].IP()) {
						log.Info("Removing IPv6 VIP immediately (required to prevent DAD failures on new leader)", "ip", cluster.Network[i].IP())
						deleted, err := cluster.Network[i].DeleteIP()
						if err != nil {
							log.Warn("delete VIP", "err", err)
						}
						if deleted {
							log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
						}
					} else {
						log.Info("Preserving IPv4 VIP address on interface, only stopped ARP broadcasting", "ip", cluster.Network[i].IP())
					}
				}
			} else {
				// Legacy behavior: delete VIP addresses on leadership loss
				log.Info("Deleting VIP addresses on leadership loss (legacy behavior)")
				for i := range cluster.Network {
					log.Info("Deleting VIP addresses on leadership loss (legacy behavior)", "i", i, "IP", cluster.Network[i].IP())
					deleted, err := cluster.Network[i].DeleteIP()
					if err != nil {
						log.Warn("delete VIP", "err", err)
					}
					if deleted {
						log.Info("deleted address", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					}
				}
			}

			log.Error("lost leadership, restarting kube-vip")
			signalChan <- syscall.SIGINT
		},
		onNewLeader: func(identity string) {
			// we're notified when new leader elected
			log.Info("New leader", "leader", identity)

			// If we're not the new leader and we have VIPs preserved from previous leadership,
			// we need to clean them up to avoid conflicts.
			if identity != c.NodeName && c.PreserveVIPOnLeadershipLoss {
				log.Info("Cleaning up preserved VIPs as another node became leader", "new_leader", identity)
				for i := range cluster.Network {
					deleted, err := cluster.Network[i].DeleteIP()
					if err != nil {
						log.Warn("failed to cleanup preserved VIP", "vip", cluster.Network[i].IP(), "err", err)
					}
					if deleted {
						log.Info("cleaned up preserved VIP to avoid conflict", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface(), "new_leader", identity)
					} else {
						log.Debug("VIP was not present on this node", "IP", cluster.Network[i].IP(), "interface", cluster.Network[i].Interface())
					}
				}
			}
		},
	}

	switch c.LeaderElectionType {
	case "kubernetes", "":
		cluster.runKubernetesLeaderElectionOrDie(objLease.Ctx, run)
	case "etcd":
		if err := cluster.runEtcdLeaderElectionOrDie(objLease.Ctx, run); err != nil {
			return err
		}
	default:
		log.Info(fmt.Sprintf("LeaderElectionMode %s not supported, exiting", c.LeaderElectionType))
	}

	return nil
}

type runConfig struct {
	config    *kubevip.Config
	leaseID   string
	leaseName string
	namespace string
	sm        *Manager

	// onStartedLeading is called when this member starts leading.
	onStartedLeading func(context.Context)
	// onStoppedLeading is called when this member stops leading.
	onStoppedLeading func()
	// onNewLeader is called when the client observes a leader that is
	// not the previously observed leader. This includes the first observed
	// leader when the client starts.
	onNewLeader func(identity string)
}

func (cluster *Cluster) runKubernetesLeaderElectionOrDie(ctx context.Context, run *runConfig) {
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:        run.leaseName,
			Namespace:   run.namespace,
			Annotations: run.config.LeaseAnnotations,
		},
		Client: run.sm.KubernetesClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: run.leaseID,
		},
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
		LeaseDuration:   time.Duration(run.config.LeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(run.config.RenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(run.config.RetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run.onStartedLeading,
			OnStoppedLeading: run.onStoppedLeading,
			OnNewLeader:      run.onNewLeader,
		},
	})
}

func (cluster *Cluster) runEtcdLeaderElectionOrDie(ctx context.Context, run *runConfig) error {
	if err := etcd.RunElectionOrDie(ctx, &etcd.LeaderElectionConfig{
		EtcdConfig:           etcd.ClientConfig{Client: run.sm.EtcdClient},
		Name:                 fmt.Sprintf("%s-%s", run.namespace, run.leaseName),
		MemberID:             run.leaseID,
		LeaseDurationSeconds: int64(run.config.LeaseDuration),
		Callbacks: etcd.LeaderCallbacks{
			OnStartedLeading: run.onStartedLeading,
			OnStoppedLeading: run.onStoppedLeading,
			OnNewLeader:      run.onNewLeader,
		},
	}); err != nil {
		return fmt.Errorf("etcd leaderelection: %w", err)
	}
	return nil
}

func (sm *Manager) NodeWatcher(ctx context.Context, lb *loadbalancer.IPVSLoadBalancer, port uint16) error {
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	log.Info("Kube-Vip is watching nodes for control-plane labels")

	listOptions := metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	}

	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.RetryWatcherClient.CoreV1().Nodes().Watch(ctx, listOptions)
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
	// defer rw.Stop()

	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes Node from Annotation watcher")
			}
			// Find the node IP address (this isn't foolproof)
			for x := range node.Status.Addresses {
				if node.Status.Addresses[x].Type == v1.NodeInternalIP {
					if checkIfNodeIsReady(node) {
						err = lb.AddBackend(node.Status.Addresses[x].Address, port)
						if err != nil {
							log.Error("add IPVS backend", "err", err)
							if errors.Is(err, &utils.PanicError{}) {
								return fmt.Errorf("add IPVS backend: %w", err)
							}
						}
					} else {
						err = lb.RemoveBackend(node.Status.Addresses[x].Address, port)
						if err != nil {
							log.Error("remove IPVS backend", "err", err)
						}
					}
				}
			}
		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes Node from Annotation watcher")
			}

			// Find the node IP address (this isn't foolproof)
			for x := range node.Status.Addresses {
				if node.Status.Addresses[x].Type == v1.NodeInternalIP {
					err = lb.RemoveBackend(node.Status.Addresses[x].Address, port)
					if err != nil {
						log.Error("Del IPVS backend", "err", err)
					}
				}
			}

			log.Info("Node deleted", "name", node.Name)

		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes Nodes")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Error(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
			}

			status := statusErr.ErrStatus
			log.Error("watcher", "status", status)
		default:
		}
	}

	log.Info("Exiting Node watcher")
	return nil
}

func checkIfNodeIsReady(node *v1.Node) bool {
	if node == nil {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady {
			if condition.Status == v1.ConditionTrue {
				return true
			}
		}
	}
	return false
}
