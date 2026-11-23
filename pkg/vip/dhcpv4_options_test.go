package vip

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func TestDHCPv4ClientOptions(t *testing.T) {
	ack, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithHwAddr(net.HardwareAddr{0x02, 0x00, 0x5e, 0x00, 0x53, 0x01}),
		dhcpv4.WithYourIP(net.ParseIP("192.0.2.10")),
	)
	if err != nil {
		t.Fatalf("building ACK: %v", err)
	}

	for _, test := range []struct {
		name        string
		hostname    string
		vendorClass string
	}{
		{name: "no identity options"},
		{name: "hostname", hostname: "nginx-dhcp"},
		{name: "vendor class", vendorClass: "kube-vip"},
		{name: "hostname and vendor class", hostname: "nginx-dhcp", vendorClass: "kube-vip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewDHCPv4Client(&net.Interface{Name: "test0"}, false, "", 0, false).WithVendorClass(test.vendorClass)
			client.WithHostName(test.hostname)

			discover, err := dhcpv4.NewDiscovery(ack.ClientHWAddr, client.requestOptions()...)
			if err != nil {
				t.Fatalf("building DHCPDISCOVER: %v", err)
			}
			renew, err := dhcpv4.NewRenewFromAck(ack, client.requestOptions()...)
			if err != nil {
				t.Fatalf("building renewal DHCPREQUEST: %v", err)
			}
			for _, message := range []*dhcpv4.DHCPv4{discover, renew} {
				assertDHCPv4Option(t, message, dhcpv4.OptionHostName, test.hostname)
				assertDHCPv4Option(t, message, dhcpv4.OptionClientIdentifier, test.hostname)
				assertDHCPv4Option(t, message, dhcpv4.OptionClassIdentifier, test.vendorClass)
			}
		})
	}
}

// assertDHCPv4Option checks that the message carries option code with the
// given value, or that it does not carry the option when want is empty.
func assertDHCPv4Option(t *testing.T, message *dhcpv4.DHCPv4, code dhcpv4.OptionCode, want string) {
	t.Helper()
	got := message.Options.Get(code)
	if want == "" {
		if got != nil {
			t.Errorf("%s has %s = %q, want it unset", message.MessageType(), code, got)
		}
		return
	}
	if string(got) != want {
		t.Errorf("%s has %s = %q, want %q", message.MessageType(), code, got, want)
	}
}
