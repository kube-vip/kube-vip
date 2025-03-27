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
	"github.com/kube-vip/kube-vip/pkg/etcd"
	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"

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
func (cluster *Cluster) StartCluster(c *kubevip.Config, sm *Manager, bgpServer *bgp.Server) error {
	var err error

	log.Info("cluster membership", "namespace", c.Namespace, "lock", c.LeaseName, "id", c.NodeName)

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
		log.Info("Received termination, signaling cluster shutdown")
		// Cancel the context, which will in turn cancel the leadership
		cancel()
		// Cancel the arp context, which will in turn stop any broadcasts
	}()

	// (attempt to) Remove the virtual IP, in case it already exists

	for i := range cluster.Network {
		err = cluster.Network[i].DeleteIP()
		if err != nil {
			log.Error("could not delete virtualIP", "err", err)
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
		bgpServer, err = bgp.NewBGPServer(&c.BGPConfig, nil)
		if err != nil {
			log.Error("new BGP server", "err", err)
		}
	}

	run := &runConfig{
		config:  c,
		leaseID: c.NodeName,
		sm:      sm,
		onStartedLeading: func(ctx context.Context) { //nolint TODO: potential clean code
			// As we're leading lets start the vip service
			err := cluster.vipService(ctxArp, ctxDNS, c, sm, bgpServer)
			if err != nil {
				log.Error("starting VIP service on leader", "err", err)
			}
		},
		onStoppedLeading: func() {
			// we can do cleanup here
			log.Info("This node is becoming a follower within the cluster")

			// Stop the dns context
			cancelDNS()
			// Stop the Arp context if it is running
			cancelArp()

			// Stop the BGP server
			if bgpServer != nil {
				err := bgpServer.Close()
				if err != nil {
					log.Warn("close BGP server", "err", err)
				}
			}

			for i := range cluster.Network {
				err := cluster.Network[i].DeleteIP()
				if err != nil {
					log.Warn("delete VIP", "err", err)
				}
			}

			log.Error("lost leadership, restarting kube-vip")
			panic("") // TODO - we could also return here
		},
		onNewLeader: func(identity string) {
			// we're notified when new leader elected
			log.Info("New leader", "leader", identity)
		},
	}

	switch c.LeaderElectionType {
	case "kubernetes", "":
		cluster.runKubernetesLeaderElectionOrDie(ctx, run)
	case "etcd":
		cluster.runEtcdLeaderElectionOrDie(ctx, run)
	default:
		log.Info(fmt.Sprintf("LeaderElectionMode %s not supported, exiting", c.LeaderElectionType))
	}

	return nil
}

type runConfig struct {
	config  *kubevip.Config
	leaseID string
	sm      *Manager

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
			Name:        run.config.LeaseName,
			Namespace:   run.config.Namespace,
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

func (cluster *Cluster) runEtcdLeaderElectionOrDie(ctx context.Context, run *runConfig) {
	etcd.RunElectionOrDie(ctx, &etcd.LeaderElectionConfig{
		EtcdConfig:           etcd.ClientConfig{Client: run.sm.EtcdClient},
		Name:                 run.config.LeaseName,
		MemberID:             run.leaseID,
		LeaseDurationSeconds: int64(run.config.LeaseDuration),
		Callbacks: etcd.LeaderCallbacks{
			OnStartedLeading: run.onStartedLeading,
			OnStoppedLeading: run.onStoppedLeading,
			OnNewLeader:      run.onNewLeader,
		},
	})
}

func (sm *Manager) NodeWatcher(lb *loadbalancer.IPVSLoadBalancer, port uint16) error {
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	log.Info("Kube-Vip is watching nodes for control-plane labels")

	listOptions := metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.RetryWatcherClient.CoreV1().Nodes().Watch(context.Background(), listOptions)
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
					err = lb.AddBackend(node.Status.Addresses[x].Address, port)
					if err != nil {
						log.Error("add IPVS backend", "err", err)
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
