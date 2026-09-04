package wireguard

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestServicePortIDsKeepProtocolAndLongNamesDistinct(t *testing.T) {
	udp := v1.ServicePort{Port: 53, Protocol: v1.ProtocolUDP}
	udpID, legacyID := ServicePortIDs("default", "dns", udp)
	if udpID != "default_dns_p53_udp" || legacyID != "default_dns_p53" {
		t.Fatalf("ServicePortIDs() = %q, %q", udpID, legacyID)
	}
	tcpID, _ := ServicePortIDs("default", "dns", v1.ServicePort{Port: 53, Protocol: v1.ProtocolTCP})
	if tcpID == udpID {
		t.Fatal("TCP and UDP Services sharing a port received the same rule ID")
	}

	longPrefix := strings.Repeat("a", 63)
	first, _ := ServicePortIDs(longPrefix, "first", udp)
	second, _ := ServicePortIDs(longPrefix, "second", udp)
	if len(first) > maxServicePortIDLength || first == second {
		t.Fatalf("long Service IDs = %q, %q", first, second)
	}

	hyphenatedNamespace, _ := ServicePortIDs("a-b", "c", udp)
	hyphenatedName, _ := ServicePortIDs("a", "b-c", udp)
	if hyphenatedNamespace == hyphenatedName {
		t.Fatalf("distinct Services received the same rule ID %q", hyphenatedNamespace)
	}
}
