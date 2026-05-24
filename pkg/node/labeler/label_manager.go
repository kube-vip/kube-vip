package labeler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// labelOperation is the operation to perform on the node labels
type labelOperation string

// labelOperation constants
const (
	labelOperationRemove labelOperation = "remove"
	labelOperationAdd    labelOperation = "add"
)

// NewManager creates a new Label Manager for the given node
func NewManager(nodeName string, clientSet *kubernetes.Clientset) *Manager {
	return &Manager{
		nodeName:  nodeName,
		clientSet: clientSet,
	}
}

// Manager is the label Manager for the node
type Manager struct {
	// nodeName is the name of the node to manage
	nodeName string

	// clientSet is the Kubernetes client set to use
	clientSet *kubernetes.Clientset
}

// AddLabel a new label to the node
func (m *Manager) AddLabel(labels map[string]string) error {
	log.Debug("add labels to node", "node", m.nodeName)
	return m.patchNode(labelOperationAdd, labels)
}

// RemoveLabel a label from the node
func (m *Manager) RemoveLabel(labels map[string]string) error {
	log.Debug("delete label from node")
	return m.patchNode(labelOperationRemove, labels)
}

// CleanUpLabels purges the node labels
func (m *Manager) CleanUpLabels(timeout time.Duration) error {
	log.Debug("cleaning up labels for node", "node", m.nodeName, "timeout", timeout)

	// create new context for labels cleanup (independent)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// get the node
	node, err := m.clientSet.CoreV1().Nodes().Get(ctx, m.nodeName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to get node %s", m.nodeName)
	}

	// collect all labels with the prefix to remove
	labels := map[string]string{}
	for _, k := range kubevip.GetKeysForCleanup() {
		if _, ok := node.Labels[k]; ok {
			labels[k] = ""
		}
	}

	if len(labels) == 0 {
		log.Debug("no labels to remove for node", "node", m.nodeName)
		return nil
	}

	// patch the node with the labels to remove
	return m.patchNode(labelOperationRemove, labels)
}

// patchNode patches the node with the given labels
func (m *Manager) patchNode(operation labelOperation, labels map[string]string) error {
	type patchStringLabel struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	patchLabels := []patchStringLabel{}
	// generate the patch
	for k, v := range labels {
		patchLabels = append(patchLabels, patchStringLabel{
			Op: string(operation),
			// replace all slashes with ~1
			Path:  fmt.Sprintf("/metadata/labels/%s", strings.ReplaceAll(k, "/", "~1")),
			Value: v,
		})
	}

	patchData, err := json.Marshal(patchLabels)
	if err != nil {
		log.Debug("node patch marshaling failed", "err", err, "labels", labels, "patch", patchLabels)
		return errors.Wrapf(err, "node patch marshaling failed for labels %v", labels)
	}

	log.Debug("patching node",
		"node", m.nodeName,
		"patch", string(patchData),
		"operation", operation,
		"labels", labels,
		"clientSetNil", m.clientSet == nil)
	if m.clientSet == nil {
		return errors.New("kubernetes client is not initialized")
	}

	// patch node
	node, err := m.clientSet.CoreV1().Nodes().Patch(ctx, m.nodeName, types.JSONPatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		log.Debug("node patching failed", "err", err, "patchData", patchData)
		return errors.Wrapf(err, "node patching failed with patch %s", string(patchData))
	}

	log.Debug("updated", "node", m.nodeName, "labels", node.Labels)

	return nil
}
