package endpoints

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestWireguardDeleteDoesNotDereferenceNilServiceContext(t *testing.T) {
	worker := &wireguardWorker{}
	service := &v1.Service{}

	if err := worker.delete(context.Background(), service, "node-a"); err != nil {
		t.Fatalf("delete() error = %v", err)
	}
}
