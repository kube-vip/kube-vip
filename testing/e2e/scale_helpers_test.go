//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

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
