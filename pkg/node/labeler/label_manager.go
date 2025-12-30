package labeler

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// labelName is the name of the label that will be added to the node
	// it is prefix for the label key before "/"
	labelName = "service-provided.kube-vip.io"
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
func (m *Manager) AddLabel(ctx context.Context, svc *corev1.Service) error {
	log.Debug("[service] add label to node", "namespace", svc.Namespace, "name", svc.Name)
	labelKey, labelValue := generateNodeLabelKeyValue(svc)
	return m.patchNode(ctx, labelOperationAdd, map[string]string{labelKey: labelValue})
}

// RemoveLabel a label from the node
func (m *Manager) RemoveLabel(ctx context.Context, svc *corev1.Service) error {
	log.Debug("[service] delete label from node", "namespace", svc.Namespace, "name", svc.Name)
	labelKey, _ := generateNodeLabelKeyValue(svc)
	return m.patchNode(ctx, labelOperationRemove, map[string]string{labelKey: ""})
}

// clean up the node labels
func (m *Manager) CleanUpLabels(timeout time.Duration) error {
	log.Debug("cleaning up labels for node", "node", m.nodeName, "timeout", timeout)

	// create new contex for labels cleanup (independent)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// get the node
	node, err := m.clientSet.CoreV1().Nodes().Get(ctx, m.nodeName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to get node %s", m.nodeName)
	}

	// collect all labels with the prefix to remove
	labels := map[string]string{}
	for k := range node.Labels {
		if strings.HasPrefix(k, labelName) {
			labels[k] = ""
		}
	}

	if len(labels) == 0 {
		log.Debug("no labels to remove for node", "node", m.nodeName)
		return nil
	}

	// patch the node with the labels to remove
	return m.patchNode(ctx, labelOperationRemove, labels)
}

// generateNodeLabelKeyValue generates a label key and value for the given service
func generateNodeLabelKeyValue(svc *corev1.Service) (string, string) {
	addresses, _ := instance.FetchServiceAddresses(svc)

	sanitized := make([]string, len(addresses))
	for i, addr := range addresses {
		sanitized[i] = sanitizeIPForLabel(addr)
	}

	return fmt.Sprintf("%s/%s.%s", labelName, svc.Name, svc.Namespace), strings.Join(sanitized, ",")
}

// Helper function to convert IPv6 hex address without colons
func sanitizeIPForLabel(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() != nil {
		return addr
	}
	return hex.EncodeToString(ip.To16())
}

// patchNode patches the node with the given labels
func (m *Manager) patchNode(ctx context.Context, operation labelOperation, labels map[string]string) error {
	type patchStringLabel struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}

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
