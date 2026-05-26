package node

import (
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node/labeler"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"k8s.io/client-go/kubernetes"
)

// LabelManager is the interface for the node label manager
type LabelManager interface {
	Labeler
	LabelCleaner
}

type Labeler interface {
	// AddLabel adds a label to the node
	AddLabel(labels map[string]string) error

	// RemoveLabel removes the label from the node
	RemoveLabel(labels map[string]string) error
}

type LabelCleaner interface {
	// CleanUpLabels removes all labels from the node
	CleanUpLabels(timeout time.Duration) error
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
