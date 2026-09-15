package endpoints

import (
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type recordingTunnelReleaser struct {
	releases []string
}

func (r *recordingTunnelReleaser) ReleaseTunnelForVIP(vip, owner string) error {
	r.releases = append(r.releases, fmt.Sprintf("%s:%s", vip, owner))
	return nil
}

func TestWireguardClearDoesNotDereferenceNilServiceContext(t *testing.T) {
	worker := &wireguardWorker{}
	service := &v1.Service{}

	worker.clear(context.TODO(), nil, nil, service, nil)
}

func TestReleaseWireguardServiceTunnelsUsesServiceUIDOwner(t *testing.T) {
	releaser := &recordingTunnelReleaser{}
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("service-uid")},
		Spec:       v1.ServiceSpec{LoadBalancerIP: "192.0.2.10"},
	}

	releaseWireguardServiceTunnels(releaser, service)

	if len(releaser.releases) != 1 || releaser.releases[0] != "192.0.2.10:service-uid" {
		t.Fatalf("tunnel releases = %v, want [192.0.2.10:service-uid]", releaser.releases)
	}
}
