package manager

import (
	"testing"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseBgpAnnotations(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Annotations: map[string]string{}},
	}

	_, _, err := parseBgpAnnotations(node, "bgp")
	if err == nil {
		t.Fatal("Parsing BGP annotations should return an error when no annotations exist")
	}

	node.Annotations = map[string]string{
		"bgp/node-asn": "1",
		"bgp/peer-asn": "2",
		"bgp/src-ip":   "3",
	}

	bgpConfig, bgpPeer, err := parseBgpAnnotations(node, "bgp")
	if err != nil {
		t.Fatal("Parsing BGP annotations should return nil when minimum config is met")
	}

	assert.Equal(t, uint32(1), bgpConfig.AS, "bgpConfig.AS parsed incorrectly")
	assert.Equal(t, uint32(2), bgpPeer.AS, "bgpPeer.AS parsed incorrectly")
	assert.Equal(t, "3", bgpConfig.RouterID, "bgpConfig.RouterID parsed incorrectly")

	node.Annotations = map[string]string{
		"bgp/node-asn": "1",
		"bgp/peer-asn": "2",
		"bgp/src-ip":   "3",
		"bgp/peer-ip":  "1,2,3",
		"bgp/bgp-pass": "cGFzc3dvcmQ=", // password
	}

	bgpConfig, bgpPeer, err = parseBgpAnnotations(node, "bgp")
	if err != nil {
		t.Fatal("Parsing BGP annotations should return nil when minimum config is met")
	}

	bgpPeers := []bgp.Peer{
		{Address: "1", AS: uint32(2)},
		{Address: "2", AS: uint32(2)},
		{Address: "3", AS: uint32(2)},
	}
	assert.Equal(t, bgpPeers, bgpConfig.Peers, "bgpConfig.Peers parsed incorrectly")
	assert.Equal(t, "3", bgpPeer.Address, "bgpPeer.Address parsed incorrectly")
	assert.Equal(t, "password", bgpPeer.Password, "bgpPeer.Password parsed incorrectly")
}
