//go:build linux
// +build linux

package vip

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func resetARPRequest(t *testing.T, request bool) {
	t.Helper()
	arpRequest = request
}

func ethLinkParams() arpLinkParams {
	return arpLinkParams{
		hardwareType: unix.ARPHRD_ETHER,
		hwLen:        ethHwLen,
		broadcast:    ethernetBroadcast,
	}
}

func ipoibLinkParams(bcast net.HardwareAddr) arpLinkParams {
	return arpLinkParams{
		hardwareType: unix.ARPHRD_INFINIBAND,
		hwLen:        ipoibHwLen,
		broadcast:    bcast,
	}
}

func ipoibBroadcast() net.HardwareAddr {
	// 20 bytes
	return net.HardwareAddr{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}
}

func Test_gratuitousARP_ethernet(t *testing.T) {
	ip := net.ParseIP("10.0.0.1")
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	params := ethLinkParams()

	t.Run("reply when arpRequest starts true", func(t *testing.T) {
		resetARPRequest(t, true)
		m, err := gratuitousARP(ip, mac, params)
		if err != nil {
			t.Fatal(err)
		}
		if m.opcode != opARPReply {
			t.Fatalf("opcode=%d, want reply (%d)", m.opcode, opARPReply)
		}
		if m.hardwareType != unix.ARPHRD_ETHER || m.hardwareAddressLength != ethHwLen {
			t.Fatalf("header: hwType=%d hwLen=%d", m.hardwareType, m.hardwareAddressLength)
		}
		if !bytes.Equal(m.senderProtocolAddress, ip.To4()) ||
			!bytes.Equal(m.targetProtocolAddress, ip.To4()) {
			t.Fatal("sender/target IP must both be VIP")
		}
		if !bytes.Equal(m.senderHardwareAddress, mac) ||
			!bytes.Equal(m.targetHardwareAddress, mac) {
			t.Fatal("reply: target HW should equal sender MAC")
		}
	})

	t.Run("request when arpRequest starts false", func(t *testing.T) {
		resetARPRequest(t, false)
		m, err := gratuitousARP(ip, mac, params)
		if err != nil {
			t.Fatal(err)
		}
		if m.opcode != opARPRequest {
			t.Fatalf("opcode=%d, want request (%d)", m.opcode, opARPRequest)
		}
		if !bytes.Equal(m.targetHardwareAddress, ethernetBroadcast) {
			t.Fatalf("request target HW=%v, want broadcast %v", m.targetHardwareAddress, ethernetBroadcast)
		}
	})
}

func Test_gratuitousARP_ipoib(t *testing.T) {
	ip := net.ParseIP("192.168.1.50")
	bcast := ipoibBroadcast()
	mac := make(net.HardwareAddr, ipoibHwLen)
	copy(mac, bcast) // any 20-byte addr works for unit test
	params := ipoibLinkParams(bcast)

	resetARPRequest(t, true)
	m, err := gratuitousARP(ip, mac, params)
	if err != nil {
		t.Fatal(err)
	}
	if m.hardwareType != unix.ARPHRD_INFINIBAND {
		t.Fatalf("hwType=%d, want INFINIBAND", m.hardwareType)
	}
	if m.hardwareAddressLength != ipoibHwLen {
		t.Fatalf("hwLen=%d, want %d", m.hardwareAddressLength, ipoibHwLen)
	}
	b, err := m.bytes()
	if err != nil {
		t.Fatal(err)
	}
	// 8-byte header + 20 + 4 + 20 + 4
	if len(b) != 8+ipoibHwLen+4+ipoibHwLen+4 {
		t.Fatalf("wire len=%d, want %d", len(b), 8+ipoibHwLen+4+ipoibHwLen+4)
	}
}

func Test_gratuitousARP_errors(t *testing.T) {
	params := ethLinkParams()
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")

	tests := []struct {
		name string
		ip   net.IP
		mac  net.HardwareAddr
		p    arpLinkParams
	}{
		{"ipv6", net.ParseIP("2001:db8::1"), mac, params},
		{"bad mac len", net.ParseIP("10.0.0.1"), net.HardwareAddr{0x01}, params},
		{"bad broadcast len", net.ParseIP("10.0.0.1"), mac, arpLinkParams{
			hardwareType: unix.ARPHRD_ETHER,
			hwLen:        ethHwLen,
			broadcast:    net.HardwareAddr{0xff},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetARPRequest(t, true)
			if _, err := gratuitousARP(tt.ip, tt.mac, tt.p); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func Test_arpMessage_bytes_golden(t *testing.T) {
	ip := net.ParseIP("10.0.0.1")
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	resetARPRequest(t, true)

	m, err := gratuitousARP(ip, mac, ethLinkParams())
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.bytes()
	if err != nil {
		t.Fatal(err)
	}

	// Ethernet gARP reply: 28 bytes
	want := []byte{
		0x00, 0x01, 0x08, 0x00, 0x06, 0x04, 0x00, 0x02, // header
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, // sender HW
		0x0a, 0x00, 0x00, 0x01, // sender IP
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, // target HW
		0x0a, 0x00, 0x00, 0x01, // target IP
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
}

func Test_rawSendto_empty(t *testing.T) {
	err := rawSendto(-1, nil, &sockaddrLLExt{})
	if err != syscall.EINVAL {
		t.Fatalf("got %v, want EINVAL", err)
	}
}

func Test_sendARPExtended_badIndex(t *testing.T) {
	iface := &net.Interface{Index: -1, Name: "dummy"}
	m := &arpMessage{arpHeader: arpHeader{hardwareAddressLength: ipoibHwLen}}
	err := sendARPExtended(-1, iface, m, ipoibLinkParams(ipoibBroadcast()), []byte{0})
	if err == nil {
		t.Fatal("expected error for bad ifindex")
	}
}

func Test_getARPLinkParams(t *testing.T) {
	root := t.TempDir()
	restore := netClassPath
	netClassPath = root
	t.Cleanup(func() { netClassPath = restore })

	writeIface := func(name, typ, bcast string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte(typ), 0o600); err != nil {
			t.Fatal(err)
		}
		if bcast != "" {
			if err := os.WriteFile(filepath.Join(dir, "broadcast"), []byte(bcast), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("ethernet", func(t *testing.T) {
		writeIface("eth0", "1\n", "")
		p, err := getARPLinkParams("eth0")
		if err != nil {
			t.Fatal(err)
		}
		if p.hardwareType != unix.ARPHRD_ETHER || p.hwLen != ethHwLen {
			t.Fatalf("%+v", p)
		}
		if !bytes.Equal(p.broadcast, ethernetBroadcast) {
			t.Fatal("wrong broadcast")
		}
	})

	t.Run("ipoib", func(t *testing.T) {
		bcast := "00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00:11:22:33\n"
		writeIface("ib0", "32\n", bcast)
		p, err := getARPLinkParams("ib0")
		if err != nil {
			t.Fatal(err)
		}
		if p.hardwareType != unix.ARPHRD_INFINIBAND || p.hwLen != ipoibHwLen {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("ipoib bad broadcast length", func(t *testing.T) {
		writeIface("ib1", "32\n", "aa:bb:cc:dd:ee:ff\n")
		if _, err := getARPLinkParams("ib1"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing type falls back to ethernet", func(t *testing.T) {
		p, err := getARPLinkParams("missing0")
		if err != nil {
			t.Fatal(err)
		}
		if p.hardwareType != unix.ARPHRD_ETHER {
			t.Fatalf("got hwType %d", p.hardwareType)
		}
	})

	t.Run("unknown type falls back to ethernet", func(t *testing.T) {
		writeIface("wlan0", "801\n", "")
		p, err := getARPLinkParams("wlan0")
		if err != nil {
			t.Fatal(err)
		}
		if p.hardwareType != unix.ARPHRD_ETHER {
			t.Fatalf("got hwType %d", p.hardwareType)
		}
	})
}
