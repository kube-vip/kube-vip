package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/debouncer"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/metrics"
	"github.com/kube-vip/kube-vip/pkg/trafficmirror"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/workqueue"
)

const concurrentServiceEventWorkers = 4
const serviceAddressRetryDelay = time.Second

type serviceEventTask struct {
	uid types.UID
	run func() time.Duration
}

type serviceEventQueue struct {
	ctx   context.Context
	queue workqueue.TypedDelayingInterface[types.NamespacedName]
	mutex sync.Mutex
	tasks map[types.NamespacedName][]*serviceEventTask
	wg    sync.WaitGroup
}

func newServiceEventQueue(ctx context.Context, workers int) *serviceEventQueue {
	q := &serviceEventQueue{
		ctx:   ctx,
		queue: workqueue.NewTypedDelayingQueue[types.NamespacedName](),
		tasks: make(map[types.NamespacedName][]*serviceEventTask),
	}
	for range workers {
		q.wg.Go(q.run)
	}
	return q
}

func (q *serviceEventQueue) Add(key types.NamespacedName, uid types.UID, run func() time.Duration) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.ctx.Err() != nil || q.queue.ShuttingDown() {
		return
	}
	tasks := q.tasks[key]
	task := &serviceEventTask{uid: uid, run: run}
	if len(tasks) != 0 && tasks[len(tasks)-1].uid == uid {
		tasks[len(tasks)-1] = task
	} else {
		tasks = append(tasks, task)
	}
	q.tasks[key] = tasks
	q.queue.Add(key)
}

func (q *serviceEventQueue) run() {
	for {
		key, shutdown := q.queue.Get()
		if shutdown {
			return
		}
		q.runNext(key)
	}
}

func (q *serviceEventQueue) runNext(key types.NamespacedName) {
	defer q.queue.Done(key)
	for {
		task := q.nextTask(key)
		if task == nil {
			return
		}
		if q.ctx.Err() == nil {
			if retryAfter := task.run(); retryAfter > 0 {
				q.retry(key, task, retryAfter)
				return
			}
		}
	}
}

func (q *serviceEventQueue) retry(key types.NamespacedName, task *serviceEventTask, retryAfter time.Duration) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.ctx.Err() != nil || q.queue.ShuttingDown() || len(q.tasks[key]) != 0 {
		return
	}
	q.tasks[key] = []*serviceEventTask{task}
	q.queue.AddAfter(key, retryAfter)
}

func (q *serviceEventQueue) nextTask(key types.NamespacedName) *serviceEventTask {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	tasks := q.tasks[key]
	if len(tasks) == 0 {
		delete(q.tasks, key)
		return nil
	}
	task := tasks[0]
	if len(tasks) == 1 {
		delete(q.tasks, key)
	} else {
		q.tasks[key] = tasks[1:]
	}
	return task
}

func (q *serviceEventQueue) Wait() {
	q.queue.ShutDown()
	q.wg.Wait()
	q.mutex.Lock()
	defer q.mutex.Unlock()
	clear(q.tasks)
}

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (p *Processor) ServicesWatcher(ctx context.Context, serviceFunc *Callback, forcedOnly bool) error {
	// first start port mirroring if enabled
	if err := p.startTrafficMirroringIfEnabled(); err != nil {
		return err
	}
	defer func() {
		// clean up traffic mirror related config
		err := p.stopTrafficMirroringIfEnabled()
		if err != nil {
			log.Error("Stopping traffic mirroring", "err", err)
		}
	}()

	if p.config.ServiceNamespace == "" {
		// v1.NamespaceAll is actually "", but we'll stay with the const in case things change upstream
		p.config.ServiceNamespace = v1.NamespaceAll
		log.Info("(svcs) starting services watcher for all namespaces")
	} else {
		log.Info("(svcs) starting services watcher", "namespace", p.config.ServiceNamespace)
	}
	if err := p.RecoverAddresses(ctx); err != nil {
		log.Warn("skipping kube-vip address recovery", "err", err)
	}

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return utils.WatchWithAuthRetry(ctx, func(ctx context.Context) (watch.Interface, error) {
				return p.rwClientSet.CoreV1().Services(p.config.ServiceNamespace).Watch(ctx, metav1.ListOptions{})
			})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}

	d, err := debouncer.New(rw.ResultChan(), p.config.DebounceTime)
	if err != nil {
		return fmt.Errorf("failed to create debouncer for endpoints event: %w", err)
	}

	var wg sync.WaitGroup
	watcherCtx, cancelWatcher := context.WithCancelCause(ctx)
	eventQueue := newServiceEventQueue(watcherCtx, concurrentServiceEventWorkers)
	defer func() {
		if d != nil {
			d.Stop()
		}
		rw.Stop()
		eventQueue.Wait()
		wg.Wait()
	}()
	defer cancelWatcher(nil)

	wg.Go(func() {
		if d != nil {
			if err := d.Start(watcherCtx); err != nil {
				log.Error("(svcs) debouncer, cancelling context", "error", err.Error())
				cancelWatcher(utils.WrapPanicError(err, "service debouncer failed"))
			}
		}
		<-watcherCtx.Done()
		log.Debug("(svcs) watcher context cancelled")
		if d != nil {
			d.Stop()
		}
		rw.Stop()
		p.Stop()
	})

	ch := rw.ResultChan()
	if d != nil {
		ch = d.Output()
	}

	// Used for tracking an active endpoint / pod
	for event := range ch {
		metrics.CountServiceWatchEvent.With(prometheus.Labels{"type": string(event.Type)}).Add(1)

		switch event.Type {
		case watch.Added, watch.Modified, watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok || svc == nil {
				log.Error("service watcher event failed", "type", event.Type, "error", "unable to parse Kubernetes Service")
				continue
			}
			if event.Type == watch.Deleted && serviceMatchesWatcher(svc, forcedOnly) {
				if _, err := p.cancelPublishedServiceContext(svc.UID); err != nil {
					log.Error("failed to cancel deleted Service context", "service", svc.Name, "namespace", svc.Namespace, "error", err)
				}
			}
			event := event
			key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
			eventQueue.Add(key, svc.UID, func() time.Duration {
				if err := p.processServiceEvent(watcherCtx, event, serviceFunc, forcedOnly, &wg, cancelWatcher); err != nil {
					if errors.Is(err, errServiceAddressPending) {
						return serviceAddressRetryDelay
					}
					cancelWatcher(err)
				}
				return 0
			})
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")
			watchErr := utils.WatchError(event.Object)
			log.Error("services", "err", watchErr)
			return utils.WrapPanicError(watchErr, "service watch failed")
		default:
		}
	}

	if ctx.Err() != nil {
		return nil
	}
	if watcherErr := context.Cause(watcherCtx); watcherErr != nil {
		return watcherErr
	}
	log.Warn("Stopping watching services for type: LoadBalancer in all namespaces")
	return utils.NewPanicError("service watch channel closed unexpectedly")
}

func (p *Processor) processServiceEvent(ctx context.Context, event watch.Event, serviceFunc *Callback, forcedOnly bool,
	wg *sync.WaitGroup, cancelWatcher context.CancelCauseFunc) error {
	var err error
	switch event.Type {
	case watch.Added, watch.Modified:
		err = p.Reconcile(ctx, event, serviceFunc, forcedOnly, wg, cancelWatcher)
		if utils.IsPanicError(err) {
			return fmt.Errorf("reconcile service error: %w", err)
		}
	case watch.Deleted:
		err = p.Delete(event, forcedOnly)
		if utils.IsPanicError(err) {
			return fmt.Errorf("delete service error: %w", err)
		}
	}
	if err != nil {
		if errors.Is(err, errServiceAddressPending) {
			return err
		}
		log.Error("service watcher event failed", "type", event.Type, "error", err)
	}
	return nil
}

func lbClassFilterLegacy(svc *v1.Service, config *kubevip.Config) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass != nil {
		// if this isn't nil then it has been configured, check if it the kube-vip loadBalancer class
		if *svc.Spec.LoadBalancerClass != config.LoadBalancerClassName {
			log.Info("(svcs) specified the wrong loadBalancer class", "service name", svc.Name, "lbClass", *svc.Spec.LoadBalancerClass)
			return true
		}
	} else if config.LoadBalancerClassOnly {
		// if kube-vip is configured to only recognize services with kube-vip's lb class, then ignore the services without any lb class
		log.Info("(svcs) kube-vip configured to only recognize services with kube-vip's lb class but the service didn't specify any loadBalancer class, ignoring", "service name", svc.Name)
		return true
	}
	return false
}

func lbClassFilter(svc *v1.Service, config *kubevip.Config) bool {
	if svc == nil {
		log.Info("(svcs) service is nil, ignoring")
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && config.LoadBalancerClassName != "" {
		log.Info("(svcs) no loadBalancer class, ignoring", "service name", svc.Name, "expected lbClass", config.LoadBalancerClassName)
		return true
	}
	if svc.Spec.LoadBalancerClass == nil && config.LoadBalancerClassName == "" {
		return false
	}
	if *svc.Spec.LoadBalancerClass != config.LoadBalancerClassName {
		log.Info("(svcs) specified wrong loadBalancer class, ignoring", "service name", svc.Name, "wrong lbClass", *svc.Spec.LoadBalancerClass, "expected lbClass", config.LoadBalancerClassName)
		return true
	}
	return false
}

func (p *Processor) serviceInterface() string {
	svcIf := p.config.Interface
	if p.config.ServicesInterface != "" {
		svcIf = p.config.ServicesInterface
	}
	return svcIf
}

func (p *Processor) startTrafficMirroringIfEnabled() error {
	if p.config.MirrorDestInterface != "" {
		svcIf := p.serviceInterface()
		log.Info("mirroring traffic", "src", svcIf, "dest", p.config.MirrorDestInterface)
		if err := trafficmirror.MirrorTrafficFromNIC(svcIf, p.config.MirrorDestInterface); err != nil {
			return err
		}
	} else {
		log.Debug("skip starting traffic mirroring since it's not enabled.")
	}
	return nil
}

func (p *Processor) stopTrafficMirroringIfEnabled() error {
	if p.config.MirrorDestInterface != "" {
		svcIf := p.serviceInterface()
		log.Info("clean up qdisc config", "interface", svcIf)
		if err := trafficmirror.CleanupQDSICFromNIC(svcIf); err != nil {
			return err
		}
	} else {
		log.Debug("skip stopping traffic mirroring since it's not enabled.")
	}
	return nil
}
