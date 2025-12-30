package providers

import (
	"context"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	watchtools "k8s.io/client-go/tools/watch"
)

type Provider interface {
	CreateRetryWatcher(context.Context, *kubernetes.Clientset,
		*v1.Service) (*watchtools.RetryWatcher, error)
	GetAllEndpoints() ([]string, error)
	GetLocalEndpoints(string, *kubevip.Config) ([]string, error)
	GetLabel() string
	UpdateServiceAnnotation(context.Context, string, string, *v1.Service, *kubernetes.Clientset) error
	LoadObject(runtime.Object, context.CancelFunc) error
}
