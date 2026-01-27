package worker

import (
	"context"
	"fmt"
	log "log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type Common struct {
	arpMgr       *arp.Manager
	cpCluster    *cluster.Cluster
	intfMgr      *networkinterface.Manager
	config       *kubevip.Config
	closing      *atomic.Bool
	signalChan   chan os.Signal
	svcProcessor *services.Processor
	mutex        *sync.Mutex
	clientSet    *kubernetes.Clientset
}

func (c *Common) InitControlPlane() error {
	var err error
	c.cpCluster, err = cluster.InitCluster(c.config, false, c.intfMgr, c.arpMgr)
	if err != nil {
		return fmt.Errorf("cluster initialization error: %w", err)
	}
	return nil
}

func (c *Common) ServicesPerServiceLeader(ctx context.Context) error {
	log.Info("beginning watching services, leaderelection will happen for every service")
	err := c.svcProcessor.StartServicesWatchForLeaderElection(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (c *Common) ServicesGlobalLeader(ctx context.Context, id string) {
	ns, err := returnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace, dropping to config", "namespace", c.config.Namespace)
		ns = c.config.Namespace
	}

	log.Info("beginning services leadership", "namespace", ns, "lock name", c.config.ServicesLeaseName, "id", id)
	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      c.config.ServicesLeaseName,
			Namespace: ns,
		},
		Client: c.clientSet.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
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
		LeaseDuration:   time.Duration(c.config.LeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(c.config.RenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(c.config.RetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				err = c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
				if err != nil {
					log.Error("service watcher", "err", err)
					if !c.closing.Load() {
						c.signalChan <- syscall.SIGINT
					}
				}
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				c.mutex.Lock()
				defer c.mutex.Unlock()
				log.Info("leader lost", "new leader", id)
				c.svcProcessor.Stop()

				log.Error("lost services leadership, restarting kube-vip")
				if !c.closing.Load() {
					c.signalChan <- syscall.SIGINT
				}
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if c.config.EnableNodeLabeling {
					applyNodeLabel(ctx, c.clientSet, c.config.Address, id, identity)
				}
				if identity == id {
					// I just got the lock
					return
				}
				log.Info("new leader elected", "new leader", identity)
			},
		},
	})
}

func (c *Common) ServicesNoLeader(ctx context.Context) error {
	log.Info("beginning watching services without leader election")
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		return fmt.Errorf("error while watching services: %w", err)
	}
	return nil
}

func returnNameSpace() (string, error) {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("unable to find Namespace")
}
