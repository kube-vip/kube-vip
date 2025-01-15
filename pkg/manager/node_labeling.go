package manager

import (
	"context"
	"encoding/json"
	"fmt"

	log "github.com/gookit/slog"
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
		log.Errorf("can't query node %s labels. error: %v", id, err)
		return
	}

	log.Debugf("node %s labels: %+v", id, node.Labels)

	value, ok := node.Labels[nodeLabelIndex]
	path := fmt.Sprintf("/metadata/labels/%s", nodeLabelJSONPath)
	log.Debugf("Received identity: %s - id: %s", identity, id)
	if ok && value == address {
		log.Debugf("removing node label `has-ip=%s` on %s", address, id)
		// Remove label
		applyPatchLabels(ctx, clientSet, id, "remove", path, address)
	} else {
		log.Debugf("setting node label `has-ip=%s` on %s", address, id)
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
		log.Errorf("node patch marshaling failed. error: %v", err)
		return
	}
	// patch node
	node, err := clientSet.CoreV1().Nodes().Patch(ctx,
		name, types.JSONPatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		log.Errorf("can't patch node %s. error: %v", name, err)
		return
	}
	log.Debugf("updated node %s labels: %+v", name, node.Labels)
}
