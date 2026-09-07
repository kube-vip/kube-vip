package services

import (
	"errors"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
)

func TestCallbackRunWithoutFunction(t *testing.T) {
	for _, callback := range []*Callback{nil, {}} {
		if err := callback.Run(nil, nil, nil); err != nil {
			t.Fatalf("Run error = %v", err)
		}
	}
}

func TestCallbackRunPropagatesLeaderElectionFlag(t *testing.T) {
	want := errors.New("callback error")
	callback := NewCallback(func(_ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup, usesLeaderElection bool) error {
		if !usesLeaderElection {
			t.Fatal("callback did not receive leader election flag")
		}
		return want
	}, true)
	if err := callback.Run(nil, nil, nil); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
}
