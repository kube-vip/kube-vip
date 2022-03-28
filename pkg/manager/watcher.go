package manager

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/prometheus/client_golang/prometheus"
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
	var wg sync.WaitGroup

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
		sm.countServiceWatchEvent.With(prometheus.Labels{"type": string(event.Type)}).Add(1)

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			// log.Debugf("Endpoints for service [%s] have been Created or modified", s.service.ServiceName)
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}
			if svc.Annotations["kube-vip.io/ignore"] == "true" {
				log.Infof("Service [%s] has an ignore annotation for kube-vip", svc.Name)
				break
			}

			log.Infof("Service [%s] has been added/modified it has an assigned external addresses [%s]", svc.Name, svc.Spec.LoadBalancerIP)
			wg.Add(1)
			err = sm.syncServices(svc, &wg)
			if err != nil {
				log.Error(err)
			}
			wg.Wait()
		case watch.Deleted:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes services from API watcher")
			}
			if svc.Annotations["kube-vip.io/ignore"] == "true" {
				log.Infof("Service [%s/%s] has an ignore annotation for kube-vip", svc.Namespace, svc.Name)
				break
			}
			err = sm.deleteService(string(svc.UID))
			if err != nil {
				log.Error(err)
			}
			log.Infof("Service [%s/%s] has been deleted", svc.Namespace, svc.Name)

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
	log.Infof("Kube-Vip is waiting for annotation prefix [%s] to be present on this node", sm.config.Annotations)
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/hostname": hostname}}
	listOptions := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}

	// First we'll check the annotations for the node and if
	// they aren't what are expected, we'll drop into the watch until they are
	nodeList, err := sm.clientSet.CoreV1().Nodes().List(context.Background(), listOptions)
	if err != nil {
		return err
	}

	// We'll assume there's only one node with the hostname annotation. If that's not true,
	// there's probably bigger problems
	node := nodeList.Items[0]

	bgpConfig, bgpPeer, err := parseBgpAnnotations(&node, sm.config.Annotations)
	if err == nil {
		// No error, the annotations already exist
		sm.config.BGPConfig = bgpConfig
		sm.config.BGPPeerConfig = bgpPeer
		return nil
	}

	// We got an error with the annotations, falling back to the watch until
	// they're as needed
	log.Warn(err)

	rw, err := watchtools.NewRetryWatcher(node.ResourceVersion, &cache.ListWatch{
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
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes Node from Annotation watcher")
			}

			bgpConfig, bgpPeer, err := parseBgpAnnotations(node, sm.config.Annotations)
			if err != nil {
				log.Error(err)
				continue
			}

			sm.config.BGPConfig = bgpConfig
			sm.config.BGPPeerConfig = bgpPeer

			rw.Stop()
		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Kubernetes Node from Kubernetes watcher")
			}

			log.Infof("Node [%s] has been deleted", node.Name)

		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Error("Error attempting to watch Kubernetes Nodes")

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

// parseNodeAnnotations parses the annotations on the node and updates the configuration
// returning an error if the annotations are not valid or missing; and nil if everything is OK
// to continue
func parseBgpAnnotations(node *v1.Node, prefix string) (bgp.Config, bgp.Peer, error) {
	bgpConfig := bgp.Config{}
	bgpPeer := bgp.Peer{}

	nodeASN := node.Annotations[fmt.Sprintf("%s/node-asn", prefix)]
	if nodeASN == "" {
		return bgpConfig, bgpPeer, fmt.Errorf("node-asn value missing or empty")
	}

	u64, err := strconv.ParseUint(nodeASN, 10, 32)
	if err != nil {
		return bgpConfig, bgpPeer, err
	}

	bgpConfig.AS = uint32(u64)

	srcIP := node.Annotations[fmt.Sprintf("%s/src-ip", prefix)]
	if srcIP == "" {
		return bgpConfig, bgpPeer, fmt.Errorf("src-ip value missing or empty")
	}

	// Set the routerID (Unique ID for BGP) to the source IP
	// Also set the BGP peering to the sourceIP
	bgpConfig.RouterID, bgpConfig.SourceIP = srcIP, srcIP

	peerASN := node.Annotations[fmt.Sprintf("%s/peer-asn", prefix)]
	if peerASN == "" {
		return bgpConfig, bgpPeer, fmt.Errorf("peer-asn value missing or empty")
	}

	u64, err = strconv.ParseUint(peerASN, 10, 32)
	if err != nil {
		return bgpConfig, bgpPeer, err
	}

	bgpPeer.AS = uint32(u64)

	peerIPString := node.Annotations[fmt.Sprintf("%s/peer-ip", prefix)]

	peerIPs := strings.Split(peerIPString, ",")

	for _, peerIP := range peerIPs {
		ipAddr := strings.TrimSpace(peerIP)

		if ipAddr != "" {
			bgpPeer.Address = ipAddr
			// Check if we're also expecting a password for this peer
			base64BGPPassword := node.Annotations[fmt.Sprintf("%s/bgp-pass", prefix)]
			if base64BGPPassword != "" {
				// Decode base64 encoded string
				decodedPassword, err := base64.StdEncoding.DecodeString(base64BGPPassword)
				if err != nil {
					return bgpConfig, bgpPeer, err
				}
				// Set the password for each peer
				bgpPeer.Password = string(decodedPassword)
			}
			bgpConfig.Peers = append(bgpConfig.Peers, bgpPeer)
		}
	}

	log.Debugf("BGPConfig: %v\n", bgpConfig)
	log.Debugf("BGPPeerConfig: %v\n", bgpPeer)

	return bgpConfig, bgpPeer, nil
}
