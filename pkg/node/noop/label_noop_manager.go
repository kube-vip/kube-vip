package noop

import (
	"context"
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

func (m *Manager) Labels(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func (m *Manager) RemoveLabelWithContext(context.Context, map[string]string) error {
	return nil
}

func (m *Manager) CleanUpLabels(context.Context, []string) error {
	return nil
}
