package leaderelection

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// MockResourceLock is a test implementation of resourcelock.Interface
type MockResourceLock struct {
	getCallCount    int
	createCallCount int
	updateCallCount int
	shouldFail      bool
	failureError    error
}

func (m *MockResourceLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	m.getCallCount++
	if m.shouldFail {
		return nil, nil, m.failureError
	}
	return &resourcelock.LeaderElectionRecord{}, []byte("test"), nil
}

func (m *MockResourceLock) Create(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	m.createCallCount++
	if m.shouldFail {
		return m.failureError
	}
	return nil
}

func (m *MockResourceLock) Update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	m.updateCallCount++
	if m.shouldFail {
		return m.failureError
	}
	return nil
}

func (m *MockResourceLock) RecordEvent(string) {}

func (m *MockResourceLock) Identity() string {
	return "test-identity"
}

func (m *MockResourceLock) Describe() string {
	return "test-lock"
}

func TestRetryableResourceLock_Get_Success(t *testing.T) {
	mock := &MockResourceLock{shouldFail: false}
	retryable := &RetryableLeaderElection{
		maxRetries:  2,
		retryDelays: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	}

	wrapper := &RetryableResourceLock{
		Interface: mock,
		retryable: retryable,
	}

	ctx := context.Background()
	_, _, err := wrapper.Get(ctx)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if mock.getCallCount != 1 {
		t.Errorf("Expected 1 call to Get, got %d", mock.getCallCount)
	}
}

func TestRetryableResourceLock_Get_RetryOnRetryableError(t *testing.T) {
	mock := &MockResourceLock{
		shouldFail:   true,
		failureError: errors.New("client rate limiter Wait returned an error: context deadline exceeded"),
	}
	retryable := &RetryableLeaderElection{
		maxRetries:  2,
		retryDelays: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	}

	wrapper := &RetryableResourceLock{
		Interface: mock,
		retryable: retryable,
	}

	ctx := context.Background()
	_, _, err := wrapper.Get(ctx)

	// Should have retried the maximum number of times plus the initial attempt
	expectedCalls := 1 + retryable.maxRetries
	if mock.getCallCount != expectedCalls {
		t.Errorf("Expected %d calls to Get, got %d", expectedCalls, mock.getCallCount)
	}

	// Should still return an error after all retries
	if err == nil {
		t.Error("Expected an error after all retries failed")
	}
}

func TestRetryableResourceLock_Update_RetryOnRetryableError(t *testing.T) {
	mock := &MockResourceLock{
		shouldFail:   true,
		failureError: errors.New("net/http: request canceled (Client.Timeout exceeded while awaiting headers)"),
	}
	retryable := &RetryableLeaderElection{
		maxRetries:  2,
		retryDelays: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	}

	wrapper := &RetryableResourceLock{
		Interface: mock,
		retryable: retryable,
	}

	ctx := context.Background()
	err := wrapper.Update(ctx, resourcelock.LeaderElectionRecord{})

	// Should have retried the maximum number of times plus the initial attempt
	expectedCalls := 1 + retryable.maxRetries
	if mock.updateCallCount != expectedCalls {
		t.Errorf("Expected %d calls to Update, got %d", expectedCalls, mock.updateCallCount)
	}

	// Should still return an error after all retries
	if err == nil {
		t.Error("Expected an error after all retries failed")
	}
}

func TestRetryableResourceLock_NoRetryOnNonRetryableError(t *testing.T) {
	mock := &MockResourceLock{
		shouldFail:   true,
		failureError: errors.New("permission denied"),
	}
	retryable := &RetryableLeaderElection{
		maxRetries:  2,
		retryDelays: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	}

	wrapper := &RetryableResourceLock{
		Interface: mock,
		retryable: retryable,
	}

	ctx := context.Background()
	err := wrapper.Update(ctx, resourcelock.LeaderElectionRecord{})

	// Should not retry for non-retryable errors
	if mock.updateCallCount != 1 {
		t.Errorf("Expected 1 call to Update, got %d", mock.updateCallCount)
	}

	// Should return the original error
	if err == nil {
		t.Error("Expected an error")
	}
}

func TestIsRetryableError(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "context deadline exceeded",
			err:      errors.New("context deadline exceeded"),
			expected: true,
		},
		{
			name:     "client rate limiter error",
			err:      errors.New("client rate limiter Wait returned an error: context deadline exceeded"),
			expected: true,
		},
		{
			name:     "timeout exceeded",
			err:      errors.New("Client.Timeout exceeded while awaiting headers"),
			expected: true,
		},
		{
			name:     "connection refused",
			err:      errors.New("connection refused"),
			expected: true,
		},
		{
			name:     "non-retryable error",
			err:      errors.New("permission denied"),
			expected: false,
		},
		{
			name:     "timed out waiting",
			err:      errors.New("timed out waiting for the condition"),
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isRetryableError(tc.err)
			if result != tc.expected {
				t.Errorf("Expected %v for error %q, got %v", tc.expected, tc.err, result)
			}
		})
	}
}
