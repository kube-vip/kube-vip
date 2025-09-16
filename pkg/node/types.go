package node

import (
	"context"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/node/labeler"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// LabelManager is the interface for the node label manager
type LabelManager interface {
	// AddLabel adds a label to the node for the given service
	AddLabel(ctx context.Context, svc *v1.Service) error

	// RemoveLabel removes the label from the node for the given service
	RemoveLabel(ctx context.Context, svc *v1.Service) error

	// CleanUpLabels removes all labels from the node
	CleanUpLabels(timeout time.Duration) error
}

// NewManager creates a new Label Manager for the given node
// NoOp implementation is returned if node labeling is disabled, or if
// running in control plane mode, or if the client is not ready
func NewManager(config *kubevip.Config, clientSet *kubernetes.Clientset) LabelManager {
	if !config.EnableNodeLabeling {
		return noop.NewManager()
	}

	if config.EnableControlPlane {
		log.Debug("Skip node labeling, control plane mode enabled")
		return noop.NewManager()
	}

	if clientSet == nil {
		log.Debug("Skip node labeling, client is not ready")
		return noop.NewManager()
	}

	log.Debug("Node labeling enabled")

	return labeler.NewManager(config.NodeName, clientSet)
}
