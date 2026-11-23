package providers

import (
	"context"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	watchtools "k8s.io/client-go/tools/watch"
)

// retryWatcherInitialResourceVersion is a local sentinel. RetryWatcher requires
// a non-empty, non-zero initial value, while an Endpoint watch must start with
// an unset resourceVersion so the API server sends the current objects. The
// provider removes this sentinel before the request; after the first event,
// RetryWatcher replaces it with the real resourceVersion to use on reconnect.
const retryWatcherInitialResourceVersion = "kube-vip-initial"

type Provider interface {
	CreateRetryWatcher(context.Context, *kubernetes.Clientset,
		*v1.Service) (*watchtools.RetryWatcher, error)
	GetAllEndpoints() ([]string, error)
	GetLocalEndpoints(string, *kubevip.Config) ([]string, error)
	GetLabel() string
	UpdateServiceAnnotation(context.Context, string, string, *v1.Service, *kubernetes.Clientset) error
	LoadObject(runtime.Object, context.CancelFunc) error
	DeleteObject(runtime.Object) error
	// ResolvePort resolves a service port to the actual target port.
	// For named ports, it looks up the port number from the endpoint.
	// For numeric ports, it returns the port as-is.
	ResolvePort(servicePort v1.ServicePort) int32
}

// EndpointNodeProvider exposes usable endpoint addresses grouped by their node.
// Endpoints without a NodeName are deliberately excluded: Pod hostnames are not
// necessarily Kubernetes node names.
type EndpointNodeProvider interface {
	GetEndpointNodes() map[string][]string
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
