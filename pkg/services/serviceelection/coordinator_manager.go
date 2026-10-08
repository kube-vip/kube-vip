package serviceelection

import (
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/kube-vip/kube-vip/pkg/lease"
)

type coordinatorManager struct {
	mutex           sync.Mutex
	coordinators    map[string]*coordinator
	nextMemberToken atomic.Uint64
	dependencies    Dependencies
}

func newCoordinatorManager(dependencies Dependencies) *coordinatorManager {
	return &coordinatorManager{
		coordinators: make(map[string]*coordinator),
		dependencies: dependencies,
	}
}

func (cm *coordinatorManager) getOrCreate(id lease.ID) *coordinator {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	if cm.coordinators == nil {
		cm.coordinators = make(map[string]*coordinator)
	}
	key := id.NamespacedName()
	if coordinator := cm.coordinators[key]; coordinator != nil {
		return coordinator
	}

	coordinator := newCoordinator(cm, id)
	cm.coordinators[key] = coordinator
	return coordinator
}

func (cm *coordinatorManager) nextToken() string {
	return strconv.FormatUint(cm.nextMemberToken.Add(1), 10)
}

func (cm *coordinatorManager) current(id lease.ID) *coordinator {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	return cm.coordinators[id.NamespacedName()]
}

func (cm *coordinatorManager) remove(coordinator *coordinator) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	if cm.coordinators[coordinator.id.NamespacedName()] == coordinator {
		delete(cm.coordinators, coordinator.id.NamespacedName())
	}
}
