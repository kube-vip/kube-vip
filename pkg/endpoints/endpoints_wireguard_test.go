package endpoints

import (
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestWireguardClearDoesNotDereferenceNilServiceContext(t *testing.T) {
	worker := &wireguardWorker{}
	service := &v1.Service{}

	worker.clear(nil, nil, service)
}
