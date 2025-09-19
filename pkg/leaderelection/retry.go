package leaderelection

import (
	"context"
	"strings"
	"time"

	log "log/slog"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// RetryableLeaderElection wraps the k8s leader election with retry logic
type RetryableLeaderElection struct {
	config      leaderelection.LeaderElectionConfig
	maxRetries  int
	retryDelays []time.Duration
}

// NewRetryableLeaderElection creates a new retryable leader election wrapper
func NewRetryableLeaderElection(config leaderelection.LeaderElectionConfig) *RetryableLeaderElection {
	return &RetryableLeaderElection{
		config:      config,
		maxRetries:  2,
		retryDelays: []time.Duration{1 * time.Second, 2 * time.Second},
	}
}

// isRetryableError checks if an error is one that we should retry
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Check for certain error patterns that indicate a retryable failure
	retryablePatterns := []string{
		"context deadline exceeded",
		"client.timeout exceeded while awaiting headers",
		"client rate limiter wait returned an error",
		"timed out waiting for the condition",
		"connection refused",
		"connection reset by peer",
		"no such host",
		"temporary failure in name resolution",
		"i/o timeout",
		"network is unreachable",
		"the object has been modified; please apply your changes to the latest version",
		"operation cannot be fulfilled",
		"resource version conflict",
		"conflict",
	}

	for _, pattern := range retryablePatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isOptimisticLockError checks if an error is specifically an optimistic locking conflict
func isOptimisticLockError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Check for optimistic locking conflict patterns
	optimisticLockPatterns := []string{
		"the object has been modified; please apply your changes to the latest version",
		"operation cannot be fulfilled",
		"resource version conflict",
	}

	for _, pattern := range optimisticLockPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// RetryableResourceLock wraps a resource lock to add retry logic
type RetryableResourceLock struct {
	resourcelock.Interface
	retryable                        *RetryableLeaderElection
	lastUpdateWasOptimisticLockError bool
}

// Get wraps the original Get with retry logic
func (r *RetryableResourceLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	for attempt := 0; attempt <= r.retryable.maxRetries; attempt++ {
		if attempt > 0 {
			delay := r.retryable.retryDelays[attempt-1]
			log.Info("retrying lease Get operation", "attempt", attempt, "delay", delay, "lock", r.Interface.Describe())
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(delay):
				// Continue with retry
			}
		}

		record, raw, err := r.Interface.Get(ctx)
		if err != nil && isRetryableError(err) && attempt < r.retryable.maxRetries {
			log.Warn("retryable error in lease Get operation", "err", err, "attempt", attempt+1, "lock", r.Interface.Describe())
			continue
		}
		return record, raw, err
	}
	return nil, nil, context.DeadlineExceeded
}

// Create wraps the original Create with retry logic
func (r *RetryableResourceLock) Create(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	for attempt := 0; attempt <= r.retryable.maxRetries; attempt++ {
		if attempt > 0 {
			delay := r.retryable.retryDelays[attempt-1]
			log.Info("retrying lease Create operation", "attempt", attempt, "delay", delay, "lock", r.Interface.Describe())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				// Continue with retry
			}
		}

		err := r.Interface.Create(ctx, ler)
		if err != nil && isRetryableError(err) && attempt < r.retryable.maxRetries {
			log.Warn("retryable error in lease Create operation", "err", err, "attempt", attempt+1, "lock", r.Interface.Describe())
			continue
		}
		return err
	}
	return context.DeadlineExceeded
}

// Update wraps the original Update with retry logic
func (r *RetryableResourceLock) Update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	for attempt := 0; attempt <= r.retryable.maxRetries; attempt++ {
		if attempt > 0 {
			// Use shorter delays for optimistic locking conflicts
			var delay time.Duration
			if attempt-1 < len(r.retryable.retryDelays) {
				delay = r.retryable.retryDelays[attempt-1]
			} else {
				delay = r.retryable.retryDelays[len(r.retryable.retryDelays)-1]
			}

			// For optimistic lock conflicts, use shorter delays
			if r.lastUpdateWasOptimisticLockError {
				delay = delay / 4 // Use shorter delay for conflicts since it's not a network failure
			}

			log.Info("retrying lease Update operation", "attempt", attempt, "delay", delay, "lock", r.Interface.Describe())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				// Continue with retry
			}
		}

		// For optimistic lock errors, try to refresh the lease record first
		currentLer := ler
		if attempt > 0 && r.lastUpdateWasOptimisticLockError {
			if latest, _, err := r.Interface.Get(ctx); err == nil && latest != nil {
				log.Info("refreshing lease record before retry", "attempt", attempt, "lock", r.Interface.Describe())
				// Update the lease record with latest data but keep our identity and timing
				currentLer = *latest
				currentLer.HolderIdentity = ler.HolderIdentity
				currentLer.RenewTime = ler.RenewTime
				currentLer.AcquireTime = ler.AcquireTime
			}
		}

		err := r.Interface.Update(ctx, currentLer)
		r.lastUpdateWasOptimisticLockError = isOptimisticLockError(err)

		if err != nil && isRetryableError(err) && attempt < r.retryable.maxRetries {
			if r.lastUpdateWasOptimisticLockError {
				log.Warn("optimistic lock conflict in lease Update operation", "err", err, "attempt", attempt+1, "lock", r.Interface.Describe())
			} else {
				log.Warn("retryable error in lease Update operation", "err", err, "attempt", attempt+1, "lock", r.Interface.Describe())
			}
			continue
		}
		return err
	}
	return context.DeadlineExceeded
}

// RunWithRetry runs leader election with retry logic for API client errors
func (r *RetryableLeaderElection) RunWithRetry(ctx context.Context) {
	// Create a retryable resource lock wrapper
	retryableLock := &RetryableResourceLock{
		Interface: r.config.Lock,
		retryable: r,
	}

	// Create a modified config with the retryable lock
	wrappedConfig := r.config
	wrappedConfig.Lock = retryableLock

	// Wrap the callbacks to add logging and retry detection
	originalCallbacks := r.config.Callbacks
	wrappedConfig.Callbacks = leaderelection.LeaderCallbacks{
		OnStartedLeading: func(ctx context.Context) {
			log.Info("started leading with retry wrapper", "lock", retryableLock.Interface.Describe())
			originalCallbacks.OnStartedLeading(ctx)
		},
		OnStoppedLeading: func() {
			log.Info("stopped leading with retry wrapper", "lock", retryableLock.Interface.Describe())
			originalCallbacks.OnStoppedLeading()
		},
		OnNewLeader: func(identity string) {
			log.Info("new leader elected with retry wrapper", "leader", identity, "lock", retryableLock.Interface.Describe())
			originalCallbacks.OnNewLeader(identity)
		},
	}

	// Run the leader election with the wrapped lock
	leaderelection.RunOrDie(ctx, wrappedConfig)
}

// RunOrDieWithRetry is a function that runs leader election with retries
func RunOrDieWithRetry(ctx context.Context, config leaderelection.LeaderElectionConfig) {
	retryable := NewRetryableLeaderElection(config)
	retryable.RunWithRetry(ctx)
}

// RunWithRetryFunc is a function that creates and runs retryable leader election
func RunWithRetryFunc(ctx context.Context, config leaderelection.LeaderElectionConfig) {
	retryable := NewRetryableLeaderElection(config)
	retryable.RunWithRetry(ctx)
}
