package services

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type upnpTestTransport func(*http.Request) (*http.Response, error)

func (f upnpTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUPNPRefreshStopsWithServiceContext(t *testing.T) {
	// A blocked status request holds the refresh lock without gateway discovery.
	started := make(chan struct{}, 1)
	client, err := kubernetes.NewForConfig(&rest.Config{
		Host: "http://kubernetes.test",
		Transport: upnpTestTransport(func(req *http.Request) (*http.Response, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web", UID: "web"}}
	inst := &instance.Instance{ServiceUID: service.UID, ServiceSnapshot: service}
	processor := &Processor{
		clientSet:        client,
		serviceLock:      newTestServiceLocks(),
		ServiceInstances: []*instance.Instance{inst},
	}
	publishTestServiceContext(processor, service)
	modeCtx, cancelMode := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	defer func() {
		cancelMode()
		wg.Wait()
	}()
	wg.Go(func() { processor.refreshUPNPForward(modeCtx, inst) })
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("UPnP refresh did not start its status request")
	}

	stopped := make(chan struct{})
	wg.Go(func() {
		processor.Stop()
		close(stopped)
	})
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Service teardown waited for a refresh with an active mode context")
	}
	if err := modeCtx.Err(); err != nil {
		t.Fatalf("Service teardown canceled the mode context: %v", err)
	}
}

func TestMapUPNPForwardsCancelsGatewayRequest(t *testing.T) {
	for _, action := range []string{"AddPortMapping", "GetExternalIPAddress"} {
		t.Run(action, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			wantAction := `"` + internetgateway2.URN_WANIPConnection_1 + "#" + action + `"`
			transport := upnpTestTransport(func(req *http.Request) (*http.Response, error) {
				// goupnp writes SOAPACTION directly without canonicalizing the header name.
				for name, values := range req.Header {
					if !strings.EqualFold(name, "SOAPAction") || !slices.Contains(values, wantAction) {
						continue
					}
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-req.Context().Done():
						return nil, req.Context().Err()
					case <-release:
						return nil, context.Canceled
					}
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">` +
						`<s:Body><AddPortMappingResponse/></s:Body></s:Envelope>`)),
				}, nil
			})
			gateway := &internetgateway2.WANIPConnection1{ServiceClient: goupnp.ServiceClient{
				SOAPClient: &soap.SOAPClient{
					EndpointURL: url.URL{Scheme: "http", Host: "gateway.test"},
					HTTPClient:  http.Client{Transport: transport},
				},
			}}
			inst := &instance.Instance{
				ServiceSnapshot: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "web"},
					Spec: v1.ServiceSpec{
						LoadBalancerIP: "192.0.2.2",
						Ports:          []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
					},
				},
				UPNPGatewayIPs: []string{"198.51.100.1"},
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				mapUPNPForwards(ctx, inst, []upnp.Gateway{{ConnectionClient: gateway}})
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("gateway request did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("gateway request did not stop on cancellation")
			}
			if !slices.Equal(inst.UPNPGatewayIPs, []string{"198.51.100.1"}) {
				t.Errorf("canceled refresh replaced gateway IPs with %v", inst.UPNPGatewayIPs)
			}
		})
	}
}
