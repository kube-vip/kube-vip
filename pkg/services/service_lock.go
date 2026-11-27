package services

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/keymutex"
)

const concurrentServiceLocks = 128

// ServiceLock serializes operations for a Service UID using a bounded pool of
// keyed mutexes.
type ServiceLock struct {
	mutex keymutex.KeyMutex
}

// NewServiceLock creates a lock striped across Service UIDs.
func NewServiceLock() *ServiceLock {
	return &ServiceLock{mutex: keymutex.NewHashed(concurrentServiceLocks)}
}

// Lock acquires the mutex associated with uid.
func (l *ServiceLock) Lock(uid types.UID) {
	l.mutex.LockKey(string(uid))
}

// Unlock releases the mutex associated with uid.
func (l *ServiceLock) Unlock(uid types.UID) error {
	if err := l.mutex.UnlockKey(string(uid)); err != nil {
		return fmt.Errorf("unlock service %q: %w", uid, err)
	}
	return nil
}
