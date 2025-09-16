package noop

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// NewManager creates a new NoOp label manager
func NewManager() *Manager {
	return &Manager{}
}

type Manager struct{}

func (m *Manager) AddLabel(_ context.Context, _ *corev1.Service) error {
	return nil
}

func (m *Manager) RemoveLabel(_ context.Context, _ *corev1.Service) error {
	return nil
}

func (m *Manager) CleanUpLabels(_ time.Duration) error {
	return nil
}
