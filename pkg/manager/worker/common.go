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
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/services"
	"k8s.io/client-go/kubernetes"
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
	id           string
	electionMgr  *election.Manager
}

func (c *Common) InitControlPlane() error {
	var err error
	c.cpCluster, err = cluster.InitCluster(c.config, false, c.intfMgr, c.arpMgr)
	if err != nil {
		return fmt.Errorf("cluster initialization error: %w", err)
	}
	return nil
}

func (c *Common) PerServiceLeader(ctx context.Context) error {
	log.Info("beginning watching services, leaderelection will happen for every service")
	err := c.svcProcessor.StartServicesWatchForLeaderElection(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (c *Common) GlobalLeader(ctx context.Context, id string, leaseName string) {
	runGlobalElection(ctx, c, leaseName, c.config, id, c.electionMgr)
}

func (c *Common) ServicesNoLeader(ctx context.Context) error {
	log.Info("beginning watching services without leader election")
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		return fmt.Errorf("error while watching services: %w", err)
	}
	return nil
}

func (c *Common) OnStartedLeading(ctx context.Context) {
	err := c.svcProcessor.ServicesWatcher(ctx, c.svcProcessor.SyncServices)
	if err != nil {
		log.Error("service watcher", "err", err)
		if !c.closing.Load() {
			c.signalChan <- syscall.SIGINT
		}
	}
}

func (c *Common) OnStoppedLeading() {
	// we can do cleanup here
	c.mutex.Lock()
	defer c.mutex.Unlock()
	log.Info("leader lost", "new leader", c.id)
	c.svcProcessor.Stop()

	log.Error("lost services leadership, restarting kube-vip")
	if !c.closing.Load() {
		c.signalChan <- syscall.SIGINT
	}
}

func (c *Common) OnNewLeader(identity string) {
	// we're notified when new leader elected
	if c.config.EnableNodeLabeling {
		labelCtx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		applyNodeLabel(labelCtx, c.clientSet, c.config.Address, c.id, identity)
	}
	if identity == c.id {
		// I just got the lock
		return
	}
	log.Info("new leader elected", "new leader", identity)
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

func runGlobalElection(ctx context.Context, a election.Actions, leaseName string,
	config *kubevip.Config, id string, electionManager *election.Manager) {
	ns, err := returnNameSpace()
	if err != nil {
		log.Warn("unable to auto-detect namespace, dropping to config", "namespace", config.Namespace)
		ns = config.Namespace
	}

	run := &election.RunConfig{
		Config:           config,
		LeaseID:          id,
		Namespace:        ns,
		LeaseName:        leaseName,
		LeaseAnnotations: map[string]string{},
		Mgr:              electionManager,
		OnStartedLeading: a.OnStartedLeading,
		OnStoppedLeading: a.OnStoppedLeading,
		OnNewLeader:      a.OnNewLeader,
	}

	if err := election.RunOrDie(ctx, run, config); err != nil {
		log.Error("leaderelection failed", "err", err, "id", id, "name", leaseName)
	}
}
