package services

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestServicesWatcherRejectsNilCallback(t *testing.T) {
	processor := &Processor{}
	if err := processor.ServicesWatcher(context.Background(), nil, false); !errors.Is(err, errServiceCallbackRequired) {
		t.Fatalf("ServicesWatcher() error = %v, want %v", err, errServiceCallbackRequired)
	}
}

func TestReconcileRejectsNilCallback(t *testing.T) {
	processor := &Processor{lbClassFilter: func(*v1.Service, *kubevip.Config) bool { return false }}
	event := watch.Event{Type: watch.Added, Object: &v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer}}}
	if err := processor.Reconcile(context.Background(), event, nil, false, nil, nil); !errors.Is(err, errServiceCallbackRequired) {
		t.Fatalf("Reconcile() error = %v, want %v", err, errServiceCallbackRequired)
	}
}

func TestCallbackReturnsFunctionError(t *testing.T) {
	want := errors.New("callback error")
	callback := Callback(func(_ *servicecontext.Context, _ *v1.Service, _ *sync.WaitGroup) error {
		return want
	})
	if err := callback(nil, nil, nil); !errors.Is(err, want) {
		t.Fatalf("callback error = %v, want %v", err, want)
	}
}
