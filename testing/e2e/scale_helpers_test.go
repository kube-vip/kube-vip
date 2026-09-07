//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestValidateScaleTopology(t *testing.T) {
	if err := ValidateScaleTopology(3, 0, 3); err != nil {
		t.Fatal(err)
	}
	for _, topology := range [][3]int{{1, 0, 3}, {2, 0, 3}, {3, 1, 3}, {3, 0, 2}, {4, 0, 4}} {
		if err := ValidateScaleTopology(topology[0], topology[1], topology[2]); err == nil {
			t.Fatalf("ValidateScaleTopology(%d, %d, %d) unexpectedly succeeded", topology[0], topology[1], topology[2])
		}
	}
}

func TestCreateScaleServiceAndLeaseNames(t *testing.T) {
	client := fake.NewClientset()
	created, err := CreateScaleService(context.Background(), client, "scale", "election", "service-00", "192.0.2.10",
		corev1.ServiceExternalTrafficPolicyTypeCluster, true, "lease-00")
	if err != nil {
		t.Fatal(err)
	}
	if created.Annotations[kubevip.ForcePerServiceElection] != "true" {
		t.Fatalf("force-election annotation = %q, want true", created.Annotations[kubevip.ForcePerServiceElection])
	}
	if created.Annotations[kubevip.ServiceLease] != "lease-00" {
		t.Fatalf("lease annotation = %q, want lease-00", created.Annotations[kubevip.ServiceLease])
	}
	leaseNames, err := ScaleServiceLeaseNames([]*corev1.Service{created}, "scale")
	if err != nil {
		t.Fatal(err)
	}
	if len(leaseNames) != 1 || leaseNames[0] != "lease-00" {
		t.Fatalf("ScaleServiceLeaseNames() = %v, want [lease-00]", leaseNames)
	}
}

func TestScaleBackendRetriesConflict(t *testing.T) {
	const namespace = "scale"
	initialReplicas := int32(1)
	client := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: scaleBackendName, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &initialReplicas},
	})
	gets := 0
	client.PrependReactor("get", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	updates := 0
	client.PrependReactor("update", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, scaleBackendName, errors.New("conflict"))
		}
		return false, nil, nil
	})

	if err := ScaleBackend(context.Background(), client, namespace, 3); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("deployment updates = %d, want 2", updates)
	}
	if gets != 2 {
		t.Fatalf("deployment gets = %d, want 2", gets)
	}
	deployment, err := client.AppsV1().Deployments(namespace).Get(context.Background(), scaleBackendName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 3 {
		t.Fatalf("deployment replicas = %v, want 3", deployment.Spec.Replicas)
	}
}

func TestScaleServiceLeaseNames(t *testing.T) {
	tests := []struct {
		name      string
		services  []*corev1.Service
		want      []string
		wantError string
	}{
		{
			name:     "default name",
			services: []*corev1.Service{NewScaleService("scale", "election", "service-00", "192.0.2.10", corev1.ServiceExternalTrafficPolicyTypeCluster, true, "")},
			want:     []string{"kubevip-service-00"},
		},
		{
			name: "configured names preserve service order",
			services: []*corev1.Service{
				NewScaleService("scale", "election", "service-00", "192.0.2.10", corev1.ServiceExternalTrafficPolicyTypeCluster, true, "lease-00"),
				NewScaleService("scale", "election", "service-01", "192.0.2.11", corev1.ServiceExternalTrafficPolicyTypeCluster, true, "lease-01"),
			},
			want: []string{"lease-00", "lease-01"},
		},
		{
			name:      "other namespace",
			services:  []*corev1.Service{NewScaleService("scale", "election", "service-00", "192.0.2.10", corev1.ServiceExternalTrafficPolicyTypeCluster, true, "other/lease-00")},
			wantError: "uses lease namespace",
		},
		{
			name: "shared lease",
			services: []*corev1.Service{
				NewScaleService("scale", "election", "service-00", "192.0.2.10", corev1.ServiceExternalTrafficPolicyTypeCluster, true, "shared"),
				NewScaleService("scale", "election", "service-01", "192.0.2.11", corev1.ServiceExternalTrafficPolicyTypeCluster, true, "shared"),
			},
			wantError: "reuses lease",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ScaleServiceLeaseNames(test.services, "scale")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("ScaleServiceLeaseNames() error = %v, want error containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("ScaleServiceLeaseNames() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestScaleCounterDelta(t *testing.T) {
	metric := func(value float64) ScaleMetricSnapshot {
		return ScaleMetricSnapshot{"node": {`counter{namespace="test"}`: value}}
	}
	empty := ScaleMetricSnapshot{"node": {}}
	tests := []struct {
		name      string
		before    ScaleMetricSnapshot
		after     ScaleMetricSnapshot
		wantDelta float64
		wantError string
	}{
		{name: "absent before and after", before: empty, after: empty},
		{name: "initialized to zero", before: empty, after: metric(0)},
		{name: "absent before nonzero after", before: empty, after: metric(1), wantError: "presence changed"},
		{name: "absent after", before: metric(1), after: empty, wantError: "presence changed"},
		{name: "absent after zero", before: metric(0), after: empty, wantError: "presence changed"},
		{name: "stable", before: metric(2), after: metric(2)},
		{name: "reset", before: metric(5), after: metric(2), wantError: "reset"},
		{name: "increment", before: metric(2), after: metric(5), wantDelta: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delta, err := ScaleCounterDelta(test.before, test.after, "counter", map[string]string{"namespace": "test"})
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("ScaleCounterDelta() error = %v", err)
				}
				if delta != test.wantDelta {
					t.Fatalf("ScaleCounterDelta() delta = %v, want %v", delta, test.wantDelta)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ScaleCounterDelta() error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestScaleTransitionDeltaReportsMetricPresence(t *testing.T) {
	const metric = "kube_vip_leader_election_transitions_total"
	before := ScaleMetricSnapshot{"node": {metric + `{lease_name="lease"}`: 4}}
	after := ScaleMetricSnapshot{"node": {metric + `{lease_name="lease"}`: 6}}

	if delta, found := ScaleTransitionDelta(before, after, []string{"lease"}); delta != 2 || !found {
		t.Fatalf("ScaleTransitionDelta() = (%v, %t), want (2, true)", delta, found)
	}
	if delta, found := ScaleTransitionDelta(before, after, []string{"missing"}); delta != 0 || found {
		t.Fatalf("ScaleTransitionDelta() for missing series = (%v, %t), want (0, false)", delta, found)
	}
}

func TestScaleServiceContenders(t *testing.T) {
	serviceMetrics := func(services ...string) map[string]float64 {
		metrics := make(map[string]float64)
		for _, service := range services {
			labels := `{name="` + service + `",namespace="scale"}`
			metrics["kube_vip_service_election_loops"+labels] = 1
			metrics["kube_vip_service_election_attempts_total"+labels] = 1
		}
		return metrics
	}
	snapshot := ScaleMetricSnapshot{
		"control-plane":  serviceMetrics("service-00", "service-01"),
		"control-plane2": serviceMetrics("service-00", "service-01"),
		"control-plane3": serviceMetrics("service-00"),
	}

	contenders, err := scaleServiceContenders(snapshot, "scale", []string{"service-00", "service-01"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(contenders, ","), "control-plane,control-plane2"; got != want {
		t.Fatalf("scaleServiceContenders() = %q, want %q", got, want)
	}
	if _, err := scaleServiceContenders(snapshot, "scale", []string{"service-00", "service-01"}, 3); err == nil || !strings.Contains(err.Error(), "only 2 nodes") {
		t.Fatalf("scaleServiceContenders() error = %v, want insufficient contender diagnostic", err)
	}
	delete(snapshot["control-plane2"], `kube_vip_service_election_attempts_total{name="service-01",namespace="scale"}`)
	if _, err := scaleServiceContenders(snapshot, "scale", []string{"service-00", "service-01"}, 2); err == nil || !strings.Contains(err.Error(), "only 1 nodes") {
		t.Fatalf("scaleServiceContenders() error = %v, want missing-attempt diagnostic", err)
	}
	snapshot["control-plane2"] = serviceMetrics("service-00", "service-01")
	snapshot["control-plane2"][`kube_vip_service_election_attempts_total{name="service-01",namespace="scale"}`] = 0
	if _, err := scaleServiceContenders(snapshot, "scale", []string{"service-00", "service-01"}, 2); err == nil || !strings.Contains(err.Error(), "only 1 nodes") {
		t.Fatalf("scaleServiceContenders() error = %v, want zero-attempt diagnostic", err)
	}
}

func TestValidateScaleLeaseTransfer(t *testing.T) {
	tests := []struct {
		name          string
		currentHolder string
		wantError     string
	}{
		{name: "different eligible holder", currentHolder: "control-plane2"},
		{name: "same holder", currentHolder: "control-plane", wantError: "still held by suppressed node"},
		{name: "unknown holder", currentHolder: "other", wantError: "ineligible holder"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateScaleLeaseTransfer("lease-00", "control-plane", test.currentHolder, []string{"control-plane", "control-plane2"})
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateScaleLeaseTransfer() error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestValidateScaleVIPOwners(t *testing.T) {
	tests := []struct {
		name        string
		owners      []string
		requireSole bool
		wantError   string
	}{
		{name: "replacement advertises during abrupt failure", owners: []string{"control-plane", "control-plane2"}},
		{name: "replacement is sole owner after restore", owners: []string{"control-plane2"}, requireSole: true},
		{name: "stale owner remains after restore", owners: []string{"control-plane", "control-plane2"}, requireSole: true, wantError: "want sole owner"},
		{name: "replacement does not advertise", owners: []string{"control-plane"}, wantError: "want owner"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateScaleVIPOwners("192.0.2.10", "control-plane2", test.owners, test.requireSole)
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateScaleVIPOwners() error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}
