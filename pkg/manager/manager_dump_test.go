package manager

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/stretchr/testify/assert"
)

func TestDumpConfiguration(t *testing.T) {
	config := &kubevip.Config{
		Address:            "192.168.1.100",
		Interface:          "eth0",
		Port:               6443,
		EnableARP:          true,
		EnableBGP:          false,
		EnableControlPlane: true,
		EnableServices:     true,
		LeaderElectionType: "kubernetes",
		Namespace:          "kube-system",
		NodeName:           "test-node",
		KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
			EnableLeaderElection: true,
			LeaseName:            "test-lease",
		},
	}

	mgr := &Manager{
		config: config,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	mgr.dumpConfiguration(context.TODO())

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	assert.NoError(t, err, "io.Copy should not return error")
	output := buf.String()

	assert.Contains(t, output, "KUBE-VIP CONFIGURATION DUMP", "should contain header")
	assert.Contains(t, output, "Node Name: test-node", "should contain node name")
	assert.Contains(t, output, "VIP: 192.168.1.100", "should contain VIP address")
	assert.Contains(t, output, "Interface: eth0", "should contain interface")
	assert.Contains(t, output, "Port: 6443", "should contain port")
	assert.Contains(t, output, "ARP Enabled: true", "should contain ARP status")
	assert.Contains(t, output, "BGP Enabled: false", "should contain BGP status")
}

func TestDumpConfigSection(t *testing.T) {
	config := &kubevip.Config{
		Address:            "192.168.1.100",
		Interface:          "eth0",
		Port:               6443,
		VIPSubnet:          "/24",
		EnableARP:          true,
		EnableBGP:          false,
		EnableControlPlane: true,
		EnableServices:     true,
		LeaderElectionType: "kubernetes",
		Namespace:          "kube-system",
		KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
			EnableLeaderElection: true,
			LeaseName:            "test-lease",
		},
	}

	mgr := &Manager{config: config}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	mgr.dumpConfigSection()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	assert.NoError(t, err, "io.Copy should not return error")
	output := buf.String()

	assert.Contains(t, output, "--- BASIC CONFIGURATION ---")
	assert.Contains(t, output, "VIP: 192.168.1.100")
	assert.Contains(t, output, "VIP Subnet: /24")
	assert.Contains(t, output, "Interface: eth0")
	assert.Contains(t, output, "Port: 6443")
	assert.Contains(t, output, "Namespace: kube-system")
	assert.Contains(t, output, "Single Node Mode: false")
	assert.Contains(t, output, "Start As Leader: false")
}

func TestDumpBGPSection(t *testing.T) {
	t.Run("BGP disabled", func(t *testing.T) {
		config := &kubevip.Config{
			EnableBGP: false,
		}
		mgr := &Manager{config: config}

		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		mgr.dumpBGPSection()

		w.Close()
		os.Stdout = old

		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		assert.NoError(t, err, "io.Copy should not return error")
		output := buf.String()

		assert.Contains(t, output, "BGP Enabled: false")
	})

	t.Run("BGP enabled", func(t *testing.T) {
		config := &kubevip.Config{
			EnableBGP: true,
			BGPConfig: kubevip.BGPConfig{
				RouterID: "192.168.1.1",
				AS:       65000,
				Peers: []kubevip.BGPPeer{
					{Address: "192.168.1.2", AS: 65001},
					{Address: "192.168.1.3", AS: 65002},
				},
			},
		}
		mgr := &Manager{config: config}

		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		mgr.dumpBGPSection()

		w.Close()
		os.Stdout = old

		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		assert.NoError(t, err, "io.Copy should not return error")
		output := buf.String()

		assert.Contains(t, output, "BGP Enabled: true")
		assert.Contains(t, output, "BGP Router ID: 192.168.1.1")
		assert.Contains(t, output, "BGP AS: 65000")
		assert.Contains(t, output, "BGP Peers: 2")
	})
}

func TestDumpARPSection(t *testing.T) {
	t.Run("ARP disabled", func(t *testing.T) {
		config := &kubevip.Config{
			EnableARP: false,
		}
		mgr := &Manager{config: config}

		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		mgr.dumpARPSection()

		w.Close()
		os.Stdout = old

		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		assert.NoError(t, err, "io.Copy should not return error")
		output := buf.String()

		assert.Contains(t, output, "ARP Enabled: false")
	})

	t.Run("ARP enabled", func(t *testing.T) {
		config := &kubevip.Config{
			EnableARP:        true,
			ArpBroadcastRate: 5,
		}
		mgr := &Manager{config: config}

		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w

		mgr.dumpARPSection()

		w.Close()
		os.Stdout = old

		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		assert.NoError(t, err, "io.Copy should not return error")
		output := buf.String()

		assert.Contains(t, output, "ARP Enabled: true")
		assert.Contains(t, output, "ARP Broadcast Rate: 5")
	})
}

func TestDumpRuntimeSection(t *testing.T) {
	config := &kubevip.Config{
		EnableLoadBalancer:   false,
		PrometheusHTTPServer: "",
		HealthCheckPort:      0,
		EnableUPNP:           false,
		EgressClean:          false,
	}
	mgr := &Manager{config: config}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	mgr.dumpRuntimeSection()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	assert.NoError(t, err, "io.Copy should not return error")
	output := buf.String()

	assert.Contains(t, output, "--- RUNTIME STATISTICS ---", "should contain runtime section header")
	assert.Contains(t, output, "Load Balancer Enabled: false", "should contain load balancer status")
	assert.Contains(t, output, "UPNP Enabled: false", "should contain UPNP status")
}
