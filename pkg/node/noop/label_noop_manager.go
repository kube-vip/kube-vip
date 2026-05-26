package noop

import (
	"time"
)

// NewManager creates a new NoOp label manager
func NewManager() *Manager {
	return &Manager{}
}

type Manager struct{}

func (m *Manager) AddLabel(labels map[string]string) error {
	return nil
}

func (m *Manager) RemoveLabel(labels map[string]string) error {
	return nil
}

func (m *Manager) CleanUpLabels(_ time.Duration) error {
	return nil
}
