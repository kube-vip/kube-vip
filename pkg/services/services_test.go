package services

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kube-vip/kube-vip/pkg/instance"
)

// Test_upnpLeaseDurationForService tests whether the default lease duration is used, and whether the annotation
// overrides it correctly.
//
// For simplicity, this table driven test does not cover all edge cases (passing in nil instance, nil service snapshot,
// nil annotations, etc).
func Test_upnpLeaseDurationForService(t *testing.T) {
	const annotation = "kube-vip.io/upnp-lease-duration" // Validating value of kubevip.UpnpLeaseDuration.
	tcs := []struct {
		name        string
		annotations map[string]string
		want        int // in seconds
	}{
		{
			name:        "No annotation uses default",
			annotations: map[string]string{},
			want:        3600,
		},
		{
			name: "Valid annotation overrides default",
			annotations: map[string]string{
				annotation: "2h",
			},
			want: 7200,
		},
		{
			name: "Valid short annotation overrides default",
			annotations: map[string]string{
				annotation: "30m",
			},
			want: 1800,
		},
		{
			name: "Invalid annotation uses default",
			annotations: map[string]string{
				annotation: "invalid-duration",
			},
			want: 3600,
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			i := &instance.Instance{
				ServiceSnapshot: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: tc.annotations,
					},
				},
			}
			gotDuration := upnpLeaseDurationForService(i)
			got := int(gotDuration.Seconds())
			if got != tc.want {
				t.Errorf("upnpLeaseDurationForService(%+v) = %v, want %v", tc.annotations, got, tc.want)
			}
		})
	}
}
