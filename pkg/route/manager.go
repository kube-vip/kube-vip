package route

import (
	"errors"
	"fmt"
	log "log/slog"
	"sync"
	"syscall"
)

type Manager struct {
	tracker map[string]*item
	mtx     sync.Mutex
}

func NewManager() *Manager {
	return &Manager{
		tracker: make(map[string]*item),
	}
}

type item struct {
	objects map[string]bool
	route   route
}

func newItem(r route) *item {
	return &item{
		objects: make(map[string]bool),
		route:   r,
	}
}

type route interface {
	AddRoute(bool) (bool, error)
	UpdateRoutes() (bool, error)
	DeleteRoute() error
	RouteHash() string
	Interface() string
}

// Add will add route
func (m *Manager) Add(object string, r route, precheck, update bool) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	key := r.RouteHash()

	itm, exists := m.tracker[key]

	if !exists {
		m.tracker[key] = newItem(r)
		itm = m.tracker[key]

		added, err := r.AddRoute(precheck)
		if err != nil {
			if update && errors.Is(err, syscall.EEXIST) && update {
				// If route exists, but protocol is not set (e.g. the route was created by the older version
				// of kube-vip) try to update it if necessary
				isUpdated, err := r.UpdateRoutes()
				if err != nil {
					return fmt.Errorf("error updating existing route: %w", err)
				}
				if isUpdated {
					log.Debug("[RT] updated route", "path", key, "object", object, "interface", r.Interface())
				}
			} else {
				// If other error occurs, return error
				return fmt.Errorf("error adding route %q: %w", key, err)
			}
		}

		if added {
			log.Debug("[RT] added route", "path", key, "object", object)
		}
	}

	itm.objects[object] = true

	log.Debug("[RT] incremented route", "path", key, "object", object, "cnt", len(itm.objects))

	return nil
}

// Delete will delete route
func (m *Manager) Delete(object string, r route) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	key := r.RouteHash()

	itm, exists := m.tracker[key]
	if !exists {
		log.Debug("[RT] deleting route - nothing to delete", "path", key, "object", object)
		return nil
	}

	delete(itm.objects, object)

	log.Debug("[RT] decremented route", "path", key, "object", object, "cnt", len(itm.objects))

	if len(itm.objects) == 0 {
		if err := r.DeleteRoute(); err != nil {
			itm.objects[object] = true
			return fmt.Errorf("failed to delete route: %w", err)
		}
		delete(m.tracker, key)
		log.Debug("[RT] deleted route", "path", key, "object", object)
	}

	return nil
}

func (m *Manager) Clear() {
	for _, itm := range m.tracker {
		if err := itm.route.DeleteRoute(); err != nil {
			log.Warn("[RT] failed to delete route", "err", err.Error())
		}
	}
	m.tracker = make(map[string]*item)
}

func (m *Manager) Check(key string) bool {
	_, exists := m.tracker[key]
	return exists
}
