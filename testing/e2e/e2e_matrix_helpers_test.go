//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/e2e/matrix"
)

func TestMatrixLeaderCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		combo matrix.Combo
		want  bool
		lease string
	}{
		{name: "ARP control plane", combo: matrix.Combo{Mode: matrix.ModeARP, Function: matrix.FunctionCP, Election: matrix.ElectionGlobal}, want: true, lease: "plndr-cp-lock"},
		{name: "RT control plane has no election", combo: matrix.Combo{Mode: matrix.ModeRT, Function: matrix.FunctionCP, Election: matrix.ElectionNone}},
		{name: "RT global service", combo: matrix.Combo{Mode: matrix.ModeRT, Function: matrix.FunctionSvc, Election: matrix.ElectionGlobal}, want: true, lease: "plndr-svcs-lock"},
		{name: "BGP global service", combo: matrix.Combo{Mode: matrix.ModeBGP, Function: matrix.FunctionSvc, Election: matrix.ElectionGlobal}, want: true, lease: "plndr-svcs-lock"},
		{name: "BGP per-service", combo: matrix.Combo{Mode: matrix.ModeBGP, Function: matrix.FunctionSvc, Election: matrix.ElectionPerService}, want: true, lease: "kubevip-test"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldAssertMatrixLeader(test.combo, test.combo.Function != matrix.FunctionCP); got != test.want {
				t.Fatalf("shouldAssertMatrixLeader() = %t, want %t", got, test.want)
			}
			if test.want && matrixLeaderLease(test.combo, "test") != test.lease {
				t.Fatalf("matrixLeaderLease() = %q, want %q", matrixLeaderLease(test.combo, "test"), test.lease)
			}
		})
	}
}

func TestMatrixKubeadmPatches(t *testing.T) {
	t.Parallel()
	if patches := matrixKubeadmPatches("fd00::10", false); patches != nil {
		t.Fatalf("matrixKubeadmPatches() = %#v when control plane is disabled", patches)
	}
	patches := matrixKubeadmPatches("fd00::10", true)
	if len(patches) != 1 || !strings.Contains(patches[0].Patch, `value: "fd00::10"`) {
		t.Fatalf("matrixKubeadmPatches() = %#v, want IPv6 certificate SAN", patches)
	}
}

func TestAnnotateNodesIncludesPeerPort(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Annotations: map[string]string{"existing": "value"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "192.0.2.10"},
		}},
	})
	peer := e2e.BGPPeerValues{IP: "192.0.2.20", AS: 65500, Port: 1179}

	if err := annotateNodes(context.Background(), "test", client, peer, 65501); err != nil {
		t.Fatalf("annotateNodes() error = %v", err)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting patched node: %v", err)
	}
	want := map[string]string{
		"existing":                   "value",
		"test/bgp-peers-0-node-asn":  "65501",
		"test/bgp-peers-0-src-ip":    "192.0.2.10",
		"test/bgp-peers-0-peer-asn":  "65500",
		"test/bgp-peers-0-peer-ip":   "192.0.2.20",
		"test/bgp-peers-0-peer-port": "1179",
	}
	if !reflect.DeepEqual(node.Annotations, want) {
		t.Fatalf("node annotations = %#v, want %#v", node.Annotations, want)
	}
}

func TestMatrixBackendsReady(t *testing.T) {
	t.Parallel()
	ready := true
	tests := []struct {
		name     string
		provider matrix.Provider
		client   *fake.Clientset
	}{
		{
			name: "endpoints", provider: matrix.ProviderEndpoints,
			client: fake.NewSimpleClientset(&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "default"}, Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}}}}}),
		},
		{
			name: "slices", provider: matrix.ProviderSlices,
			client: fake.NewSimpleClientset(&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "service-1", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "service"}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := matrixBackendsReady(context.Background(), test.client, "default", "service", test.provider); err != nil {
				t.Fatalf("matrixBackendsReady() error = %v", err)
			}
		})
	}

	if err := matrixBackendsReady(context.Background(), fake.NewSimpleClientset(), "default", "missing", matrix.ProviderSlices); err == nil {
		t.Fatal("matrixBackendsReady() succeeded without EndpointSlices")
	}
}

func TestMatrixDualStackServiceConfiguration(t *testing.T) {
	t.Parallel()
	service := newTestService("service", "default", "backend", "192.0.2.10,2001:db8::10",
		corev1.IPFamilyPolicyPreferDualStack, matrixServiceFamilies(matrix.FamilyDual),
		corev1.ServiceExternalTrafficPolicyCluster, "", 80, false, false)
	if service.Spec.LoadBalancerIP != "" {
		t.Fatalf("legacy loadBalancerIP = %q, want empty for dual-stack service", service.Spec.LoadBalancerIP)
	}
	if got := service.Annotations[kubevip.LoadbalancerIPAnnotation]; got != "192.0.2.10,2001:db8::10" {
		t.Fatalf("load-balancer annotation = %q", got)
	}
	if service.Spec.IPFamilyPolicy == nil || *service.Spec.IPFamilyPolicy != corev1.IPFamilyPolicyPreferDualStack {
		t.Fatalf("IPFamilyPolicy = %v, want PreferDualStack", service.Spec.IPFamilyPolicy)
	}
	if got := service.Spec.IPFamilies; len(got) != 2 || got[0] != corev1.IPv4Protocol || got[1] != corev1.IPv6Protocol {
		t.Fatalf("IPFamilies = %v, want [IPv4 IPv6]", got)
	}
}

func TestMatrixManifestConfiguresBothServiceMasks(t *testing.T) {
	t.Parallel()
	path := filepath.Join("kube-vip.yaml.tmpl")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifestTemplate, err := template.New(filepath.Base(path)).Parse(string(contents))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"bgp", "arp"} {
		var rendered strings.Builder
		values := e2e.KubevipManifestValues{Mode: mode}
		if mode == "bgp" {
			values.BGPAS = 65501
		}
		if err := manifestTemplate.Execute(&rendered, values); err != nil {
			t.Fatal(err)
		}
		var pod corev1.Pod
		if err := yaml.Unmarshal([]byte(rendered.String()), &pod); err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, env := range pod.Spec.Containers[0].Env {
			if env.Name == "vip_subnet" {
				got = env.Value
			}
		}
		if got != "32,128" {
			t.Errorf("%s vip_subnet = %q, want 32,128", mode, got)
		}
	}
}
