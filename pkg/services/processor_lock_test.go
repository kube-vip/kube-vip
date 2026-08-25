package services

import (
	"context"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestServiceLocksAreScopedByUID(t *testing.T) {
	processor := &Processor{}

	t.Run("different Services proceed concurrently", func(t *testing.T) {
		unlockFirst := processor.lockService(types.UID("service-a"))
		acquired := make(chan struct{})
		go func() {
			unlockSecond := processor.lockService(types.UID("service-b"))
			close(acquired)
			unlockSecond()
		}()

		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Fatal("different Service UID was blocked by another Service lock")
		}
		unlockFirst()
	})

	t.Run("same Service remains serialized", func(t *testing.T) {
		uid := types.UID("service-a")
		unlockFirst := processor.lockService(uid)
		acquired := make(chan struct{})
		go func() {
			unlockSecond := processor.lockService(uid)
			close(acquired)
			unlockSecond()
		}()

		select {
		case <-acquired:
			t.Fatal("same Service UID acquired the lock concurrently")
		case <-time.After(50 * time.Millisecond):
		}
		unlockFirst()

		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Fatal("same Service UID remained blocked after unlock")
		}
	})
}

func TestDeleteServiceCleansUpAfterContextRemoval(t *testing.T) {
	uid := types.UID("service-a")
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid, Name: "service-a", Namespace: "default"}}
	processor := &Processor{
		config:           &kubevip.Config{EnableServicesElection: true},
		ServiceInstances: []*instance.Instance{{ServiceSnapshot: service}},
	}

	if err := processor.deleteService(context.Background(), uid, servicecontext.New(context.Background())); err != nil {
		t.Fatalf("deleteService() error = %v", err)
	}
	if got := processor.findServiceInstance(service); got != nil {
		t.Fatal("deleted Service instance remained tracked after leader cleanup")
	}
}
