package instance

import (
	"context"
	"sync"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node"
	"github.com/kube-vip/kube-vip/pkg/route"
	v1 "k8s.io/api/core/v1"
)

// Factory creates fully initialized service instances.
type Factory struct {
	config           *kubevip.Config
	interfaceManager *networkinterface.Manager
	arpManager       *arp.Manager
	routeManager     *route.Manager
	labelManager     node.Labeler
}

// NewFactory creates a service instance factory with its runtime dependencies.
func NewFactory(config *kubevip.Config, interfaceManager *networkinterface.Manager,
	arpManager *arp.Manager, routeManager *route.Manager, labelManager node.Labeler) *Factory {
	return &Factory{
		config:           config,
		interfaceManager: interfaceManager,
		arpManager:       arpManager,
		routeManager:     routeManager,
		labelManager:     labelManager,
	}
}

// Create builds and initializes an Instance for service.
func (f *Factory) Create(ctx context.Context, service *v1.Service, wg *sync.WaitGroup) (*Instance, error) {
	return NewInstance(ctx, service, f.config, f.interfaceManager, f.arpManager,
		f.routeManager, f.labelManager, wg)
}
