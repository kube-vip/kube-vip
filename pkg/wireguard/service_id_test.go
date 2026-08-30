package wireguard

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestServicePortIDsKeepProtocolAndLongNamesDistinct(t *testing.T) {
	port := v1.ServicePort{Port: 53, Protocol: v1.ProtocolUDP}
	first, legacy := ServicePortIDs("default", "dns", port)
	if first != "default_dns_p53_udp" || legacy != "default_dns_p53" {
		t.Fatalf("ServicePortIDs = %q, %q", first, legacy)
	}

	longPrefix := strings.Repeat("a", 63)
	first, _ = ServicePortIDs(longPrefix, "first", port)
	second, _ := ServicePortIDs(longPrefix, "second", port)
	if len(first) > maxServicePortIDLength || first == second {
		t.Fatalf("long IDs = %q, %q", first, second)
	}
}
