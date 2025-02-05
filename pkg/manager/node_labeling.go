package manager

import (
	"context"
	"encoding/json"
	"fmt"

	log "log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	nodeLabelIndex    = "kube-vip.io/has-ip"
	nodeLabelJSONPath = `kube-vip.io~1has-ip`
)

type patchStringLabel struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

// applyNodeLabel add/remove node label `kube-vip.io/has-ip=<VIP-Address>` to/from
// the node where the virtual IP was added to/removed from.
func applyNodeLabel(clientSet *kubernetes.Clientset, address, id, identity string) {
	ctx := context.Background()
	node, err := clientSet.CoreV1().Nodes().Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		log.Error("can't query node labels", "node", id, "err", err)
		return
	}

	log.Debug(fmt.Sprintf("node %s labels: %+v", id, node.Labels))

	value, ok := node.Labels[nodeLabelIndex]
	path := fmt.Sprintf("/metadata/labels/%s", nodeLabelJSONPath)
	log.Debug(fmt.Sprintf("Received identity: %s - id: %s", identity, id))
	if ok && value == address {
		log.Debug(fmt.Sprintf("removing node label `has-ip=%s` on %s", address, id))
		// Remove label
		applyPatchLabels(ctx, clientSet, id, "remove", path, address)
	} else {
		log.Debug(fmt.Sprintf("setting node label `has-ip=%s` on %s", address, id))
		// Append label
		applyPatchLabels(ctx, clientSet, id, "add", path, address)
	}
}

// applyPatchLabels add/remove node labels
func applyPatchLabels(ctx context.Context, clientSet *kubernetes.Clientset,
	name, operation, path, value string) {
	patchLabels := []patchStringLabel{{
		Op:    operation,
		Path:  path,
		Value: value,
	}}
	patchData, err := json.Marshal(patchLabels)
	if err != nil {
		log.Error("node patch marshaling failed", "err", err)
		return
	}
	// patch node
	node, err := clientSet.CoreV1().Nodes().Patch(ctx,
		name, types.JSONPatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		log.Error("node patch marshaling failed", "err", err)
		return
	}
	log.Debug("updated", "node", name, "labels", node.Labels)
}
