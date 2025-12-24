package services

import (
	"context"
	"fmt"

	log "log/slog"

	"github.com/davecgh/go-spew/spew"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/trafficmirror"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
)

// This function handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (p *Processor) ServicesWatcher(ctx context.Context, serviceFunc func(*servicecontext.Context, *v1.Service) error) error {
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

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return p.rwClientSet.CoreV1().Services(p.config.ServiceNamespace).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}
	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-p.shutdownChan:
			log.Debug("(svcs) shutdown called")
			// Stop the retry watcher
			rw.Stop()
			return
		case <-exitFunction:
			log.Debug("(svcs) function ending")
			// Stop the retry watcher
			rw.Stop()
			return
		}
	}()
	ch := rw.ResultChan()

	// Used for tracking an active endpoint / pod
	for event := range ch {
		p.CountServiceWatchEvent.With(prometheus.Labels{"type": string(event.Type)}).Add(1)

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			restart, err := p.AddOrModify(ctx, event, serviceFunc)
			if restart {
				break
			}
			if err != nil {
				return fmt.Errorf("add/modify service error: %w", err)
			}
		case watch.Deleted:
			restart, err := p.Delete(event)
			if restart {
				break
			}
			if err != nil {
				return fmt.Errorf("delete service error: %w", err)
			}
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Error(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
			}

			status := statusErr.ErrStatus
			log.Error("services", "err", status)
		default:
		}
	}
	close(exitFunction)
	log.Warn("Stopping watching services for type: LoadBalancer in all namespaces")
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
