//go:build linux

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/node/noop"
	"github.com/kube-vip/kube-vip/pkg/route"
	"github.com/kube-vip/kube-vip/pkg/wireguard"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestWireguardServiceInstanceSurvivesTunnelCleanup(t *testing.T) {
	parentNamespace := os.Getenv("KUBE_VIP_WIREGUARD_PARENT_NETNS")
	if parentNamespace == "" {
		namespace, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(executable, "-test.run=^TestWireguardServiceInstanceSurvivesTunnelCleanup$", "-test.v")
		command.Env = append(os.Environ(), "KUBE_VIP_WIREGUARD_PARENT_NETNS="+namespace)
		command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		output, err := command.CombinedOutput()
		if errors.Is(err, syscall.EPERM) && os.Getenv("KUBE_VIP_REQUIRE_NETNS") == "" {
			t.Skipf("creating isolated network namespace: %v", err)
		}
		if err != nil {
			t.Fatalf("isolated WireGuard lifecycle: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if namespace == parentNamespace {
		t.Fatal("WireGuard lifecycle is not running in an isolated network namespace")
	}

	for _, existingLoopbackVIP := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-loopback-vip=%t", existingLoopbackVIP), func(t *testing.T) {
			loopback, err := netlink.LinkByName("lo")
			if err != nil {
				t.Fatal(err)
			}
			if err := netlink.LinkSetUp(loopback); err != nil {
				t.Fatal(err)
			}
			if existingLoopbackVIP {
				address, err := netlink.ParseAddr("192.0.2.10/32")
				if err != nil {
					t.Fatal(err)
				}
				address.Scope = syscall.RT_SCOPE_HOST
				if err := netlink.AddrAdd(loopback, address); err != nil {
					t.Fatal(err)
				}
			}

			manager := wireguardTestManager(t)
			for _, name := range []string{"wg0", "wg1"} {
				if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
					t.Fatalf("creating startup/stale interface %s: %v", name, err)
				}
			}
			if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "unrelated0"}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				link, err := netlink.LinkByName("unrelated0")
				if err != nil {
					t.Errorf("finding test interface for cleanup: %v", err)
					return
				}
				if err := netlink.LinkDel(link); err != nil {
					t.Errorf("deleting test interface: %v", err)
				}
			})
			if err := manager.CleanupStaleResources(); err != nil {
				t.Fatalf("cleaning stale tunnels: %v", err)
			}
			for _, name := range []string{"wg0", "wg1"} {
				assertWireguardLinkAbsent(t, name)
			}
			if _, err := netlink.LinkByName("unrelated0"); err != nil {
				t.Fatalf("cleanup removed an unrelated interface: %v", err)
			}

			config := &kubevip.Config{EnableWireguard: true, Interface: "wg0", VIPSubnet: "32"}
			var workers sync.WaitGroup
			ctx, cancel := context.WithCancel(context.Background())
			defer workers.Wait()
			defer cancel()
			defer func() {
				if err := manager.TearDownAllTunnels(); err != nil {
					t.Errorf("tearing down tunnels: %v", err)
				}
			}()
			routeManager := route.NewManager()
			create := func(name, vip string) *instance.Instance {
				t.Helper()
				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
					Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, LoadBalancerIP: vip, ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
						Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
				}
				inst, err := instance.NewInstance(ctx, service, config, networkinterface.NewManager(),
					arp.NewManager(config), routeManager, noop.NewManager(), &workers)
				if err != nil {
					t.Fatalf("creating Service %s without an active tunnel: %v", name, err)
				}
				if err := endpoints.StartService(ctx, service, inst, nil, &workers); err != nil {
					t.Fatalf("starting Service %s: %v", name, err)
				}
				if err := endpoints.AcquireWireguardServiceTunnels(manager, service); err != nil {
					t.Fatalf("acquiring Service %s tunnel: %v", name, err)
				}
				assertWireguardAddress(t, "lo", "192.0.2.10", existingLoopbackVIP)
				return inst
			}
			cleanup := func(inst *instance.Instance, remaining ...*instance.Instance) {
				t.Helper()
				if err := endpoints.CleanupService(ctx, config, nil, routeManager, manager, inst, remaining); err != nil {
					t.Fatalf("cleaning Service %s: %v", inst.ServiceSnapshot.Name, err)
				}
				assertWireguardAddress(t, "lo", "192.0.2.10", existingLoopbackVIP)
			}

			first := create("first", "192.0.2.10")
			assertWireguardAddress(t, "wg0", "192.0.2.10", true)
			shared := create("shared", "192.0.2.10")
			cleanup(first, shared)
			assertWireguardAddress(t, "wg0", "192.0.2.10", true)
			cleanup(shared)
			assertWireguardLinkAbsent(t, "wg0")

			recreated := create("recreated", "192.0.2.10")
			assertWireguardAddress(t, "wg0", "192.0.2.10", true)
			cleanup(recreated)
			assertWireguardLinkAbsent(t, "wg0")

			other := create("other", "192.0.2.20")
			assertWireguardAddress(t, "wg1", "192.0.2.20", true)
			assertWireguardAddress(t, "lo", "192.0.2.20", false)
			cleanup(other)
			assertWireguardLinkAbsent(t, "wg1")
			assertWireguardAddress(t, "lo", "192.0.2.20", false)
		})
	}
}

type wireguardSecretTransport struct{ secret []byte }

func (transport wireguardSecretTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(transport.secret))), Request: request}, nil
}

func wireguardTestManager(t *testing.T) *wireguard.TunnelManager {
	t.Helper()
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peerKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var tunnels strings.Builder
	for index, address := range []string{"192.0.2.10", "192.0.2.20"} {
		fmt.Fprintf(&tunnels, "wg%d:\n  vip: %s/32\n  privateKey: %s\n  peerPublicKey: %s\n  peerEndpoint: 127.0.0.1:51830\n  listenPort: %d\n  allowedIPs: [198.51.%d.0/24]\n",
			index, address, privateKey.String(), peerKey.PublicKey().String(), 51820+index, 100+index)
	}
	secret, err := json.Marshal(&v1.Secret{Data: map[string][]byte{"tunnels": []byte(tunnels.String())}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(&rest.Config{Host: "https://kubernetes.invalid", Transport: wireguardSecretTransport{secret: secret}})
	if err != nil {
		t.Fatal(err)
	}
	manager := wireguard.NewTunnelManager()
	if err := manager.LoadConfigurationsFromSecret(context.Background(), client, "default", "wireguard"); err != nil {
		t.Fatal(err)
	}
	return manager
}

func assertWireguardLinkAbsent(t *testing.T, name string) {
	t.Helper()
	_, err := netlink.LinkByName(name)
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("interface %s remains after cleanup: %v", name, err)
	}
}

func assertWireguardAddress(t *testing.T, name, address string, want bool) {
	t.Helper()
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range addresses {
		if candidate.IP.String() == address {
			if !want {
				t.Fatalf("unexpected address %s on %s", address, name)
			}
			return
		}
	}
	if want {
		t.Fatalf("address %s missing from %s", address, name)
	}
}
