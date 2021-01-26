package manager

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/davecgh/go-spew/spew"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	watchtools "k8s.io/client-go/tools/watch"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// This file handles the watching of a services endpoints and updates a load balancers endpoint configurations accordingly
func (sm *Manager) servicesWatcher(ctx context.Context) error {
	// Watch function

	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Services(v1.NamespaceAll).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		return fmt.Errorf("error creating services watcher: %s", err.Error())
	}
	go func() {
		<-sm.signalChan
		// Cancel the context
		rw.Stop()
	}()
	ch := rw.ResultChan()
	//defer rw.Stop()
	log.Infoln("Beginning watching services for type: LoadBalancer in all namespaces")

	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}
			if svc.Spec.LoadBalancerIP == "" {
				log.Infof("Service [%s] has been addded/modified, it has no assigned external addresses", svc.Name)
			} else {
				log.Infof("Service [%s] has been addded/modified, it has an assigned external addresses [%s]", svc.Name, svc.Spec.LoadBalancerIP)
				err = sm.syncServices(svc)
				if err != nil {
					log.Error(err)
				}
			}
		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}
			err = sm.stopService(string(svc.UID))
			if err != nil {
				log.Error(err)
			}
			err = sm.deleteService(string(svc.UID))
			if err != nil {
				log.Error(err)
			}
			log.Infof("Service [%s] has been deleted", svc.Name)

		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

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
	log.Warnln("Stopping watching services for type: LoadBalancer in all namespaces")
	return nil
}

// This file handles the watching of node annotations for configuration, it will exit once the annotations are
// present
func (sm *Manager) annotationsWatcher() error {
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	log.Infoln("Kube-Vip is waiting for annotations to be present on this node")
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/hostname": hostname}}
	listOptions := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}

	rw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return sm.clientSet.CoreV1().Nodes().Watch(context.Background(), listOptions)
		},
	})
	if err != nil {
		return fmt.Errorf("error creating annotations watcher: %s", err.Error())
	}

	go func() {
		<-sm.signalChan
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
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}

			nodeASN := node.Annotations[fmt.Sprintf("%s/node-asn", sm.config.Annotations)]
			if nodeASN != "" {
				u64, err := strconv.ParseUint(nodeASN, 10, 32)
				if err != nil {
					return err
				}
				sm.config.BGPConfig.AS = uint32(u64)
			} else {
				continue
			}
			srcIP := node.Annotations[fmt.Sprintf("%s/src-ip", sm.config.Annotations)]
			if srcIP != "" {
				sm.config.BGPConfig.RouterID = srcIP
			} else {
				continue
			}
			peerASN := node.Annotations[fmt.Sprintf("%s/peer-asn", sm.config.Annotations)]
			if peerASN != "" {
				u64, err := strconv.ParseUint(peerASN, 10, 32)
				if err != nil {
					return err
				}
				sm.config.BGPPeerConfig.AS = uint32(u64)
			} else {
				continue
			}

			peerIPString := node.Annotations[fmt.Sprintf("%s/peer-ip", sm.config.Annotations)]

			peerIPs := strings.Split(peerIPString, ",")

			for _, peerIP := range peerIPs {
				ipAddr := strings.TrimSpace(peerIP)

				if ipAddr != "" {
					sm.config.BGPPeerConfig.Address = ipAddr
					sm.config.BGPConfig.Peers = append(sm.config.BGPConfig.Peers, sm.config.BGPPeerConfig)
				}
			}

			log.Debugf("%s / %d / %s / %d \n", sm.config.BGPConfig.RouterID, sm.config.BGPConfig.AS, sm.config.BGPConfig.Peers[0].Address, sm.config.BGPConfig.Peers[0].AS)

			rw.Stop()
			break
		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}

			log.Infof("Node [%s] has been deleted", node.Name)

		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes services")

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
