package manager

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"

	"github.com/davecgh/go-spew/spew"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	watchtools "k8s.io/client-go/tools/watch"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

// This file handles the watching of node annotations for configuration, it will exit once the annotations are
// present
func (sm *Manager) annotationsWatcher() error {
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	log.Info("Kube-Vip is waiting for annotation prefix to be present on this node", "prefix", sm.config.Annotations)

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/hostname": sm.config.NodeName}}
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

	bgpConfig, bgpPeer, err := parseBgpAnnotations(sm.config.BGPConfig, &node, sm.config.Annotations)
	if err == nil {
		// No error, the annotations already exist
		sm.config.BGPConfig = bgpConfig
		sm.config.BGPPeerConfig = bgpPeer
		return nil
	}

	// We got an error with the annotations, falling back to the watch until
	// they're as needed
	log.Warn(err.Error())

	rw, err := watchtools.NewRetryWatcher(node.ResourceVersion, &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return sm.rwClientSet.CoreV1().Nodes().Watch(context.Background(), listOptions)
		},
	})
	if err != nil {
		return fmt.Errorf("error creating annotations watcher: %s", err.Error())
	}

	exitFunction := make(chan struct{})
	go func() {
		select {
		case <-sm.shutdownChan:
			log.Debug("[annotations] shutdown called")
			// Stop the retry watcher
			rw.Stop()
			return
		case <-exitFunction:
			log.Debug("[annotations] function ending")
			// Stop the retry watcher
			rw.Stop()
			return
		}
	}()

	ch := rw.ResultChan()

	for event := range ch {
		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes Node from Annotation watcher")
			}

			bgpConfig, bgpPeer, err := parseBgpAnnotations(sm.config.BGPConfig, node, sm.config.Annotations)
			if err != nil {
				log.Error(err.Error())
				continue
			}

			sm.config.BGPConfig = bgpConfig
			sm.config.BGPPeerConfig = bgpPeer

			rw.Stop()
		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("unable to parse Kubernetes Node from Kubernetes watcher")
			}

			log.Info("Node has been deleted", "name", node.Name)

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
			log.Error(status.String())
		default:
		}
	}
	close(exitFunction)
	log.Info("Exiting Annotations watcher")
	return nil

}

// parseNodeAnnotations parses the annotations on the node and updates the configuration
// returning an error if the annotations are not valid or missing; and nil if everything is OK
// to continue
//
// Parsed annotation config overlays config in passed bgpConfig in order to preserve configs
// set by other means with the exception that bgpConfig.Peers is overwritten.
//
// The BGP configuration is managed through Kubernetes node annotations
// using a simple key-value format: <prefix>/<info>
//
// For example:
// * <prefix>/node-asn - The ASN (Autonomous System Number) of the node
// * <prefix>/peer-asn - The ASN of the BGP peer
// * <prefix>/peer-ip  - The IP address(es) of the BGP peer(s), comma-separated for multiple peers
// * <prefix>/src-ip   - The source IP address used for BGP communication
// * <prefix>/bgp-pass - Optional base64-encoded BGP password
//
// The prefix is configurable and typically set to 'bgp' or similar value
func parseBgpAnnotations(bgpConfig bgp.Config, node *v1.Node, prefix string) (bgp.Config, bgp.Peer, error) {
	bgpPeer := bgp.Peer{}

	nodeASN := ""
	for k, v := range node.Annotations {
		regex := regexp.MustCompile(fmt.Sprintf("^%s/node-asn$", prefix))
		if regex.Match([]byte(k)) {
			nodeASN = v
		}
	}
	if nodeASN == "" {
		return bgpConfig, bgpPeer, fmt.Errorf("node-asn value missing or empty")
	}

	u64, err := strconv.ParseUint(nodeASN, 10, 32)
	if err != nil {
		return bgpConfig, bgpPeer, err
	}

	bgpConfig.AS = uint32(u64)

	srcIP := ""
	for k, v := range node.Annotations {
		regex := regexp.MustCompile(fmt.Sprintf("^%s/(bgp-peers-0-)?src-ip", prefix))
		if regex.Match([]byte(k)) {
			srcIP = v
		}
	}
	if srcIP == "" {
		return bgpConfig, bgpPeer, fmt.Errorf("src-ip value missing or empty")
	}

	// Set the routerID (Unique ID for BGP) to the source IP
	// Also set the BGP peering to the sourceIP
	bgpConfig.RouterID, bgpConfig.SourceIP = srcIP, srcIP

	peerASN := ""
	for k, v := range node.Annotations {
		regex := regexp.MustCompile(fmt.Sprintf("^%s/(bgp-peers-0-)?peer-asn", prefix))
		if regex.Match([]byte(k)) {
			peerASN = v
		}
	}
	if peerASN == "" {
		return bgpConfig, bgpPeer, fmt.Errorf("peer-asn value missing or empty")
	}

	u64, err = strconv.ParseUint(peerASN, 10, 32)
	if err != nil {
		return bgpConfig, bgpPeer, err
	}

	bgpPeer.AS = uint32(u64)

	peerIPString := ""
	for k, v := range node.Annotations {
		regex := regexp.MustCompile(fmt.Sprintf("^%s/(bgp-peers-[0-9]+-)?peer-ip", prefix))
		if regex.Match([]byte(k)) {
			peerIPString += v + ","
		}
	}
	peerIPString = strings.TrimRight(peerIPString, ",")

	peerIPs := strings.Split(peerIPString, ",")

	bgpConfig.Peers = make([]bgp.Peer, 0, len(peerIPs))
	for _, peerIP := range peerIPs {
		ipAddr := strings.TrimSpace(peerIP)

		if ipAddr != "" {
			bgpPeer.Address = ipAddr
			// Check if we're also expecting a password for this peer
			base64BGPPassword := ""
			for k, v := range node.Annotations {
				regex := regexp.MustCompile(fmt.Sprintf("^%s/(bgp-peers-0-)?bgp-pass", prefix))
				if regex.Match([]byte(k)) {
					base64BGPPassword = v
				}
			}
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

	//log.Debugf("BGPConfig: %v\n", bgpConfig)
	//log.Debugf("BGPPeerConfig: %v\n", bgpPeer)

	return bgpConfig, bgpPeer, nil
}
