package manager

import (
	"context"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAnnotationsWatcherHandlesEmptyNodeList(t *testing.T) {
	client := fake.NewSimpleClientset()
	config := &kubevip.Config{
		NodeName:    "node-a",
		Annotations: "kube-vip.io",
	}

	if err := annotationsWatcher(context.Background(), client, client, config); err == nil {
		t.Fatal("annotationsWatcher() error = nil, want empty node-list error")
	}
}
