//go:build linux

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	kernelnft "github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/kube-vip/kube-vip/pkg/election"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/services/serviceelection"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// Exercise real client-go lease elections, Service activation/cleanup, routes
// and nftables in separate node namespaces. Only the API server and node labels
// are substituted; this does not exercise a CNI or send application traffic.
func TestRecoverEgressAfterEndpointLoss(t *testing.T) {
	if os.Getenv("KUBE_VIP_EGRESS_RECOVERY_CHILD") == "" {
		// Cluster shutdown also uses background goroutines. Isolate the whole
		// process so their default namespace cannot be the host's namespace.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		//nolint:gosec // Re-executes this test binary with fixed arguments, not an externally supplied command.
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoverEgressAfterEndpointLoss$", "-test.v", "-test.timeout=55s")
		command.Env = append(os.Environ(), "KUBE_VIP_EGRESS_RECOVERY_CHILD=1")
		command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		output, err := command.CombinedOutput()
		if err != nil {
			if !requireNetworkNamespaces && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) {
				t.Skipf("network namespaces unavailable: %v", err)
			}
			t.Fatalf("isolated recovery test: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	for _, test := range []struct {
		name            string
		nextLeader      string
		serviceElection bool
	}{
		{name: "retained instance", nextLeader: "node-a"},
		{name: "same leader", nextLeader: "node-a", serviceElection: true},
		{name: "different leader", nextLeader: "node-b", serviceElection: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "egress", Namespace: "default", UID: "egress-recovery",
					Annotations: map[string]string{kubevip.Egress: "true", kubevip.ActiveEndpoint: "10.0.0.1"},
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: "192.0.2.10",
					ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyLocal,
				},
			}
			client := newEgressRecoveryClient(t, service)
			first := newEgressRecoveryNode(t, "node-a", client, service, test.serviceElection)
			second := newEgressRecoveryNode(t, "node-b", client, service, test.serviceElection)
			first.reconcile(t, service, true)
			if test.serviceElection {
				first.start(t, service)
			} else {
				if err := inEgressRecoveryNamespace(first.namespace, func() error {
					return first.processor.SyncServices(first.ctx, service, &first.wg)
				}); err != nil {
					t.Fatal(err)
				}
				first.reconcile(t, service, true)
			}
			first.waitForDatapath(t, service, true)
			originalInstance := first.processor.findServiceInstance(service)
			if test.serviceElection {
				assertRecoveryLeaseHolder(t, client, service, "node-a")
			}

			first.reconcile(t, service, false)
			first.waitForDatapath(t, service, false)
			current, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := current.Annotations[kubevip.ActiveEndpoint]; got != "" {
				t.Errorf("zero endpoints left a stale active endpoint %q", got)
			}

			next := first
			if test.nextLeader == "node-b" {
				next = second
			}
			next.reconcile(t, current, true)
			if next == second {
				// Model the Service watch delivering the persisted endpoint before
				// the new node joins its first campaign.
				current, err = client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				next.start(t, current)
			}
			next.waitForDatapath(t, service, true)
			if !test.serviceElection && first.processor.findServiceInstance(service) != originalInstance {
				t.Fatal("recovery replaced the instance instead of reconciling its egress state")
			}
			restored, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if restored.Annotations[kubevip.ActiveEndpoint] != "10.0.0.1" ||
				restored.Annotations[kubevip.EgressNftablesTable] != "kube_vip_recovery" {
				t.Fatalf("recovery left incorrect Service annotations: %v", restored.Annotations)
			}
			if test.serviceElection {
				assertRecoveryLeaseHolder(t, client, service, test.nextLeader)
			}
			if next == second {
				first.waitForDatapath(t, service, false)
			}
			t.Logf("%s recovered the VIP route and endpoint-specific SNAT rule without restarting either processor", test.nextLeader)
		})
	}
}

type egressRecoveryNode struct {
	processor *Processor
	endpoints *endpoints.Processor
	ctx       *servicecontext.Context
	namespace netns.NsHandle
	lastKnown string
	wg        sync.WaitGroup
}

func newEgressRecoveryNode(t *testing.T, name string, client *kubernetes.Clientset, service *v1.Service, serviceElection bool) *egressRecoveryNode {
	t.Helper()
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	namespace, createErr := netns.New()
	restoreErr := netns.Set(original)
	_ = original.Close()
	runtime.UnlockOSThread()
	if restoreErr != nil {
		t.Fatalf("restore network namespace: %v", restoreErr)
	}
	if createErr != nil {
		if requireNetworkNamespaces {
			t.Fatal(createErr)
		}
		t.Skipf("network namespaces unavailable: %v", createErr)
	}
	t.Cleanup(func() { _ = namespace.Close() })
	if err := inEgressRecoveryNamespace(namespace, func() error {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "kvrecover0"}}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		return netlink.LinkSetUp(link)
	}); err != nil {
		t.Fatal(err)
	}
	config := &kubevip.Config{
		NodeName: name, InstanceName: "recovery", Interface: "kvrecover0", VIPSubnet: "32",
		EnableServicesElection: serviceElection, EnableRoutingTable: true, LeaderElectionType: "kubernetes",
		RoutingTableID: 200, RoutingTableType: syscall.RTN_LOCAL, RoutingProtocol: recoveryProtocol,
		DisableServiceUpdates: true, EgressWithNftables: true,
		EgressPodCidr: "10.0.0.0/16", EgressServiceCidr: "10.96.0.0/12",
		KubernetesLeaderElection: kubevip.KubernetesLeaderElection{LeaseDuration: 4, RenewDeadline: 3, RetryPeriod: 1},
	}
	electionManager, err := election.NewManager(config, client, client)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewServicesProcessor(config, nil, client, client, networkinterface.NewManager(), nil,
		&testLabeler{}, electionManager, lease.NewManager(), route.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	adapter := &electionAdapter{processor: processor}
	processor.electionCoordinators, err = serviceelection.NewManager(&serviceelection.Dependencies{
		Config: config, Leases: processor.leaseMgr, ElectionManager: electionManager,
		State: adapter, Datapath: &egressRecoveryDatapath{electionAdapter: adapter, namespace: namespace}, Runner: adapter, Scheduler: adapter,
	})
	if err != nil {
		t.Fatal(err)
	}
	node := &egressRecoveryNode{processor: processor, ctx: publishTestServiceContext(processor, service), namespace: namespace}
	node.endpoints = endpoints.NewEndpointProcessor(config, providers.NewEndpointslices(), nil,
		processor.findServiceInstance, nil, processor.routeMgr, processor.serviceLock)
	t.Cleanup(func() {
		if !serviceElection {
			if err := inEgressRecoveryNamespace(namespace, func() error {
				return processor.deleteServiceForContext(context.Background(), service.UID, node.ctx)
			}); err != nil {
				t.Errorf("clean service: %v", err)
			}
		}
		node.ctx.Cancel()
		node.wg.Wait()
		processor.electionCoordinators.Wait()
	})
	return node
}

func (n *egressRecoveryNode) start(t *testing.T, service *v1.Service) {
	t.Helper()
	n.wg.Go(func() {
		if err := n.processor.StartServicesLeaderElection(n.ctx, service, &n.wg); err != nil {
			t.Errorf("service election: %v", err)
		}
	})
}

func (n *egressRecoveryNode) reconcile(t *testing.T, service *v1.Service, present bool) {
	t.Helper()
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "egress-slice", Namespace: service.Namespace},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	if present {
		slice.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, NodeName: &n.processor.config.NodeName}}
	}
	err := inEgressRecoveryNamespace(n.namespace, func() error {
		_, err := n.endpoints.Reconcile(n.ctx, watch.Event{Type: watch.Modified, Object: slice}, &n.lastKnown,
			service, n.processor.config.NodeName, &n.wg, n.processor.clientSet,
			func(ctx context.Context, updated *v1.Service, inst *instance.Instance) error {
				return n.processor.updateEgressConfiguration(ctx, n.ctx, updated, inst)
			})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (n *egressRecoveryNode) waitForDatapath(t *testing.T, service *v1.Service, present bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var routeFound, snatFound, instanceFound bool
	for {
		n.processor.serviceLock.Lock(service.UID)
		instanceFound = n.processor.findServiceInstance(service) != nil
		err := inEgressRecoveryNamespace(n.namespace, func() error {
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: 200}, netlink.RT_FILTER_TABLE)
			if err != nil {
				return err
			}
			routeFound = false
			for _, r := range routes {
				if r.Dst != nil && r.Dst.String() == "192.0.2.10/32" && r.Protocol == recoveryProtocol {
					routeFound = true
				}
			}
			conn := &kernelnft.Conn{}
			chains, err := conn.ListChains()
			if err != nil {
				return err
			}
			want := nftables.GetSNatChainForTable(false, string(service.UID), "kube_vip_recovery")
			snatFound = false
			for _, chain := range chains {
				if chain.Table.Name != want.Table.Name || chain.Name != want.Name {
					continue
				}
				rules, err := conn.GetRules(chain.Table, chain)
				if err != nil {
					return err
				}
				for _, rule := range rules {
					var source, target, snat bool
					for _, expression := range rule.Exprs {
						switch e := expression.(type) {
						case *expr.Lookup:
							set, err := conn.GetSetByName(chain.Table, e.SetName)
							if err != nil {
								return err
							}
							elements, err := conn.GetSetElements(set)
							if err != nil {
								return err
							}
							for _, element := range elements {
								source = source || bytes.Equal(element.Key, net.ParseIP("10.0.0.1").To4())
							}
						case *expr.Immediate:
							target = target || bytes.Equal(e.Data, net.ParseIP("192.0.2.10").To4())
						case *expr.NAT:
							snat = e.Type == expr.NATTypeSourceNAT
						}
					}
					snatFound = snatFound || (source && target && snat)
				}
			}
			return nil
		})
		unlockErr := n.processor.serviceLock.Unlock(service.UID)
		if err != nil || unlockErr != nil {
			t.Fatal(errors.Join(err, unlockErr))
		}
		wantInstance := present || !n.processor.config.EnableServicesElection
		if routeFound == present && snatFound == present && instanceFound == wantInstance {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: route=%t SNAT=%t instance=%t, want present=%t", n.processor.config.NodeName,
				routeFound, snatFound, instanceFound, present)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type egressRecoveryDatapath struct {
	*electionAdapter
	namespace netns.NsHandle
}

func (d *egressRecoveryDatapath) Activate(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context, wg *sync.WaitGroup) error {
	return inEgressRecoveryNamespace(d.namespace, func() error { return d.electionAdapter.Activate(ctx, service, svcCtx, wg) })
}

func (d *egressRecoveryDatapath) Cleanup(ctx context.Context, service *v1.Service, svcCtx *servicecontext.Context, current func() bool) error {
	return inEgressRecoveryNamespace(d.namespace, func() error { return d.electionAdapter.Cleanup(ctx, service, svcCtx, current) })
}

func inEgressRecoveryNamespace(namespace netns.NsHandle, run func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return err
	}
	defer original.Close()
	defer func() { err = errors.Join(err, netns.Set(original)) }()
	if err := netns.Set(namespace); err != nil {
		return err
	}
	return run()
}

func assertRecoveryLeaseHolder(t *testing.T, client *kubernetes.Clientset, service *v1.Service, holder string) {
	t.Helper()
	namespace, name := lease.ServiceName(service)
	resource, err := client.CoordinationV1().Leases(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resource.Spec.HolderIdentity == nil || *resource.Spec.HolderIdentity != holder {
		t.Fatalf("lease holder = %v, want %s", resource.Spec.HolderIdentity, holder)
	}
}

// A small JSON REST transport shared by both real client-go electors. Writes
// use resourceVersion checks, including the lock's out-of-band annotation update.
func newEgressRecoveryClient(t *testing.T, service *v1.Service) *kubernetes.Clientset {
	t.Helper()
	var mu sync.Mutex
	version := 1
	service = service.DeepCopy()
	service.ResourceVersion = "1"
	objects := map[string]k8sruntime.Object{"/api/v1/namespaces/default/services/egress": service}
	client, err := kubernetes.NewForConfig(&rest.Config{
		Host: "https://egress-recovery.test", ContentConfig: rest.ContentConfig{ContentType: "application/json"},
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			path := request.URL.Path
			object := objects[path]
			status := http.StatusOK
			if request.Method == http.MethodPost || request.Method == http.MethodPut {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				updated, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
				if err != nil {
					return nil, err
				}
				metadata, err := meta.Accessor(updated)
				if err != nil {
					return nil, err
				}
				if request.Method == http.MethodPost {
					path += "/" + metadata.GetName()
					object = objects[path]
				}
				if object != nil {
					current, err := meta.Accessor(object)
					if err != nil {
						return nil, err
					}
					if request.Method == http.MethodPost || metadata.GetResourceVersion() != current.GetResourceVersion() {
						status = http.StatusConflict
						object = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonConflict, Code: int32(status)}
					}
				}
				if status == http.StatusOK {
					version++
					metadata.SetResourceVersion(strconv.Itoa(version))
					objects[path], object = updated.DeepCopyObject(), updated
				}
			}
			if object == nil {
				status = http.StatusNotFound
				object = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: int32(status)}
			}
			body, err := json.Marshal(object)
			if err != nil {
				return nil, fmt.Errorf("encode API response: %w", err)
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
