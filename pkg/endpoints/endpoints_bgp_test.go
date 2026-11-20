package endpoints

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type recordingBGPRoutes struct {
	routes  []bgp.Route
	deleted []string
	err     error
}

func (r *recordingBGPRoutes) AddHost(context.Context, string, string) error {
	return errors.New("unexpected self-next-hop advertisement")
}

func (r *recordingBGPRoutes) DelHost(_ context.Context, prefix, _ string) error {
	r.deleted = append(r.deleted, prefix)
	return r.err
}

func (r *recordingBGPRoutes) UpdateRoute(_ context.Context, route bgp.Route) error {
	r.routes = append(r.routes, route)
	return r.err
}

type bgpTestNetwork struct {
	vip.Network
	ip, cidr string
}

func (n *bgpTestNetwork) IP() string   { return n.ip }
func (n *bgpTestNetwork) CIDR() string { return n.cidr }

func TestBGPForwardingNodeEndpointChanges(t *testing.T) {
	for _, policy := range []v1.ServiceExternalTrafficPolicy{v1.ServiceExternalTrafficPolicyLocal, v1.ServiceExternalTrafficPolicyCluster} {
		for _, ipv6 := range []bool{false, true} {
			name := string(policy) + "/IPv4"
			vipIP, prefix := "198.51.100.10", "198.51.100.10/32"
			podA, podZ := "10.0.0.1", "10.1.0.1"
			nextA, nextZ := "192.0.2.1", "192.0.2.2"
			addressType := discoveryv1.AddressTypeIPv4
			if ipv6 {
				name = string(policy) + "/IPv6"
				vipIP, prefix = "2001:db8::10", "2001:db8::10/128"
				podA, podZ = "fd00::1", "fd01::1"
				nextA, nextZ = "2001:db8:1::1", "2001:db8:1::2"
				addressType = discoveryv1.AddressTypeIPv6
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				a, z := "node-a", "node-z"
				provider := providers.NewEndpointslices()
				load := func(sliceName, node, ip string) *discoveryv1.EndpointSlice {
					t.Helper()
					slice := &discoveryv1.EndpointSlice{
						ObjectMeta: metav1.ObjectMeta{Name: sliceName}, AddressType: addressType,
						Endpoints: []discoveryv1.Endpoint{{NodeName: &node, Addresses: []string{ip}}},
					}
					if err := provider.LoadObject(slice, func() {}); err != nil {
						t.Fatal(err)
					}
					return slice
				}
				load("z", z, podZ)
				sliceA := load("a", a, podA)
				recorder := &recordingBGPRoutes{}
				worker := &BGP{
					generic: newGeneric(&kubevip.Config{EnableBGP: true}, provider), bgpServer: recorder,
					client: fake.NewSimpleClientset(
						&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: a}, Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: nextA}}}},
						&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: z}, Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: nextZ}}}},
					),
				}
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", Annotations: map[string]string{kubevip.BGPForwardingNode: "true"}},
					Spec:       v1.ServiceSpec{ExternalTrafficPolicy: policy},
				}
				inst := &instance.Instance{Clusters: []*cluster.Cluster{{Network: []vip.Network{&bgpTestNetwork{ip: vipIP, cidr: prefix}}}}}
				var configured sync.Map
				endpoints, err := worker.getEndpoints(service, "node-without-endpoints")
				if err != nil || len(endpoints) != 2 {
					t.Fatalf("all speakers must see remote endpoints: %v, %v", endpoints, err)
				}
				process := func(want string) {
					t.Helper()
					if err := worker.processInstance(ctx, &configured, service, inst); err != nil {
						t.Fatal(err)
					}
					got := recorder.routes[len(recorder.routes)-1]
					if got != (bgp.Route{Prefix: prefix, NextHop: want, Object: "default/test"}) {
						t.Fatalf("route = %+v, want next hop %s", got, want)
					}
					if _, ok := configured.Load(vipIP); !ok {
						t.Fatal("successful advertisement not tracked")
					}
				}
				process(nextA)
				if err := provider.DeleteObject(sliceA); err != nil {
					t.Fatal(err)
				}
				process(nextZ) // A configured VIP must still update after failover.
				load("a", a, podA)
				process(nextA) // Selection is deterministic when the node returns.
				recorder.err = errors.New("injected update failure")
				configured.Delete(vipIP)
				if err := worker.processInstance(ctx, &configured, service, inst); !errors.Is(err, recorder.err) {
					t.Fatalf("update error = %v", err)
				}
				if _, ok := configured.Load(vipIP); ok {
					t.Fatal("failed update was marked configured")
				}
				recorder.err = nil
				// Loss of a same-family node address must withdraw the stale VIP.
				worker.client = fake.NewSimpleClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: a}}, &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: z}})
				if err := worker.processInstance(ctx, &configured, service, inst); err == nil {
					t.Fatal("missing next-hop addresses should be reported")
				}
				if len(recorder.deleted) != 1 || recorder.deleted[0] != prefix {
					t.Fatalf("stale route not withdrawn: %v", recorder.deleted)
				}
				lastKnown := ""
				worker.clear(ctx, &configured, &lastKnown, service, inst)
				if len(recorder.deleted) != 2 {
					t.Fatal("endpoint loss must withdraw the VIP")
				}
			})
		}
	}
}

func TestBGPForwardingNodeAddressFamily(t *testing.T) {
	worker := &BGP{client: fake.NewSimpleClientset(
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeExternalIP, Address: "192.0.2.100"},
			{Type: v1.NodeInternalIP, Address: "2001:db8::1"},
		}}},
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "192.0.2.2"},
		}}},
	)}
	nodes := map[string][]string{"a": {"fd00::1", "10.0.0.1"}, "b": {"10.1.0.1"}}
	for _, test := range []struct {
		ipv4 bool
		want string
	}{{true, "192.0.2.2"}, {false, "2001:db8::1"}} {
		got, err := worker.forwardingNodeAddress(context.Background(), []string{"a", "b"}, nodes, test.ipv4)
		if err != nil || got != test.want {
			t.Fatalf("IPv4=%t: next hop = %q, %v; want %q", test.ipv4, got, err, test.want)
		}
	}
}
