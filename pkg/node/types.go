package node

import (
	"context"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node/labeler"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"k8s.io/client-go/kubernetes"
)

// LabelManager is the interface for the node label manager
type LabelManager interface {
	Labeler
	LabelReader
	LabelCleaner
}

type Labeler interface {
	// AddLabel adds a label to the node
	AddLabel(labels map[string]string) error

	// RemoveLabel removes the label from the node
	RemoveLabel(labels map[string]string) error
}

type LabelReader interface {
	// Labels returns the node labels whose key starts with prefix
	Labels(ctx context.Context, prefix string) (map[string]string, error)
	// RemoveLabelWithContext removes the given labels from the node using ctx
	RemoveLabelWithContext(ctx context.Context, labels map[string]string) error
}

type LabelCleaner interface {
	// CleanUpLabels removes the given label keys from the node
	CleanUpLabels(ctx context.Context, keys []string) error
}

// NewManager creates a new Label Manager for the given node
// NoOp implementation is returned if node labeling is disabled,
// or if the client is not ready
func NewManager(config *kubevip.Config, clientSet *kubernetes.Clientset) LabelManager {
	if !config.EnableNodeLabeling {
		return noop.NewManager()
	}

	if clientSet == nil {
		log.Debug("Skip node labeling, client is not ready")
		return noop.NewManager()
	}

	log.Debug("Node labeling enabled")

	return labeler.NewManager(config.NodeName, clientSet)
}
