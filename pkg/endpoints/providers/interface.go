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
	// ResolvePort resolves a service port to the actual target port.
	// For named ports, it looks up the port number from the endpoint.
	// For numeric ports, it returns the port as-is.
	ResolvePort(servicePort v1.ServicePort) int32
}

// ResolvePortWithLookup is a helper that resolves a service port using a lookup function
// for named ports. This consolidates the common resolution logic.
func ResolvePortWithLookup(servicePort v1.ServicePort, lookupNamedPort func(string) int32) int32 {
	if servicePort.TargetPort.IntVal != 0 {
		return servicePort.TargetPort.IntVal
	}
	if servicePort.TargetPort.StrVal != "" {
		if port := lookupNamedPort(servicePort.TargetPort.StrVal); port != 0 {
			return port
		}
	}
	return servicePort.Port
}
