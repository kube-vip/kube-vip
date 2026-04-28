package election

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/etcd"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
	"github.com/kube-vip/kube-vip/pkg/utils"
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

type Manager struct {
	KubernetesClient   *kubernetes.Clientset
	RetryWatcherClient *kubernetes.Clientset
	// This channel is used to signal a shutdown

	EtcdClient *clientv3.Client
}

// NewManager will create a new managing object
func NewManager(config *kubevip.Config, k8sClientset, rwClientset *kubernetes.Clientset) (*Manager, error) {
	m := &Manager{}

	switch config.LeaderElectionType {
	case "kubernetes", "":
		if k8sClientset == nil || rwClientset == nil {
			return nil, fmt.Errorf("provided nil clientset")
		}
		m.KubernetesClient = k8sClientset
		m.RetryWatcherClient = rwClientset
	case "etcd":
		client, err := etcd.NewClient(config)
		if err != nil {
			return nil, err
		}
		m.EtcdClient = client
	default:
		return nil, fmt.Errorf("invalid LeaderElectionMode %s not supported", config.LeaderElectionType)
	}

	return m, nil
}

func RunOrDie(ctx context.Context, run *RunConfig, c *kubevip.Config) error {
	switch c.LeaderElectionType {
	case "kubernetes", "":
		runKubernetesLeaderElectionOrDie(ctx, run)
	case "etcd":
		if err := runEtcdLeaderElectionOrDie(ctx, run); err != nil {
			return err
		}
	default:
		log.Info("LeaderElectionMode not supported, exiting", "mode", c.LeaderElectionType)
	}

	return nil
}

func runKubernetesLeaderElectionOrDie(ctx context.Context, run *RunConfig) {
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:        run.LeaseID.Name(),
			Namespace:   run.LeaseID.Namespace(),
			Annotations: run.LeaseAnnotations,
		},
		Client: run.Mgr.KubernetesClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: run.Config.NodeName,
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
		LeaseDuration:   time.Duration(run.Config.LeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(run.Config.RenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(run.Config.RetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run.OnStartedLeading,
			OnStoppedLeading: run.OnStoppedLeading,
			OnNewLeader:      run.OnNewLeader,
		},
	})
}

func runEtcdLeaderElectionOrDie(ctx context.Context, run *RunConfig) error {
	if err := etcd.RunElectionOrDie(ctx, &etcd.LeaderElectionConfig{
		EtcdConfig:           etcd.ClientConfig{Client: run.Mgr.EtcdClient},
		Name:                 run.LeaseID.NamespacedName(),
		MemberID:             run.Config.NodeName,
		LeaseDurationSeconds: int64(run.Config.LeaseDuration),
		Callbacks: etcd.LeaderCallbacks{
			OnStartedLeading: run.OnStartedLeading,
			OnStoppedLeading: run.OnStoppedLeading,
			OnNewLeader:      run.OnNewLeader,
		},
	}); err != nil {
		return fmt.Errorf("etcd leaderelection: %w", err)
	}
	return nil
}

type Actions interface {
	OnStartedLeading(ctx context.Context)
	OnStoppedLeading()
	OnNewLeader(identity string)
}

type RunConfig struct {
	Config           *kubevip.Config
	LeaseID          lease.ID
	Mgr              *Manager
	LeaseAnnotations map[string]string

	// onStartedLeading is called when this member starts leading.
	OnStartedLeading func(context.Context)
	// onStoppedLeading is called when this member stops leading.
	OnStoppedLeading func()
	// onNewLeader is called when the client observes a leader that is
	// not the previously observed leader. This includes the first observed
	// leader when the client starts.
	OnNewLeader func(identity string)
}

func (em *Manager) NodeWatcher(ctx context.Context, lb *loadbalancer.IPVSLoadBalancer, port uint16) error {
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	log.Info("Kube-Vip is watching nodes for control-plane labels")

	listOptions := metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/control-plane",
	}

	wg := sync.WaitGroup{}
	defer wg.Wait()

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()

	rw, err := watchtools.NewRetryWatcherWithContext(watchCtx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return utils.WatchWithAuthRetry(ctx, func(ctx context.Context) (watch.Interface, error) {
				return em.RetryWatcherClient.CoreV1().Nodes().Watch(watchCtx, listOptions)
			})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating label watcher: %w", err)
	}

	wg.Go(func() {
		<-watchCtx.Done()
		log.Info("Node watcher context cancelled, stopping")
		// Stop the retrywatcher
		rw.Stop()
	})

	ch := rw.ResultChan()

	var watchErr error
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
							log.Error("failed to add node to load balancer", "node", node.Name, "ip", node.Status.Addresses[x].Address, "err", err)
							if errors.Is(err, &utils.PanicError{}) {
								return fmt.Errorf("add IPVS backend: %w", err)
							}
						}
					} else {
						err = lb.RemoveBackend(node.Status.Addresses[x].Address, port)
						if err != nil {
							log.Error("failed to remove node from load balancer", "node", node.Name, "ip", node.Status.Addresses[x].Address, "err", err)
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
						log.Error("failed to remove node from load balancer", "node", node.Name, "ip", node.Status.Addresses[x].Address, "err", err)
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
			watchErr = fmt.Errorf("node watcher error, status: %s", status.String())
		default:
		}
	}

	log.Info("Exiting Node watcher")
	return watchErr
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
