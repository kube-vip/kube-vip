package vip

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
)

// documentationEnterpriseNumber is the Private Enterprise Number that RFC 5612
// reserves for documentation and examples.
const documentationEnterpriseNumber = 32473

func TestDHCPv6ClientOptions(t *testing.T) {
	hwaddr := net.HardwareAddr{0x02, 0x00, 0x5e, 0x00, 0x53, 0x01}
	serverID := &dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: net.HardwareAddr{0x02, 0x00, 0x5e, 0x00, 0x53, 0x02}}
	leasedIP := dhcpv6.OptIAAddress{IPv6Addr: net.ParseIP("2001:db8::10")}

	for _, test := range []struct {
		name             string
		enterpriseNumber uint32
		vendorClass      string
	}{
		{name: "no vendor class"},
		{name: "vendor class", enterpriseNumber: documentationEnterpriseNumber, vendorClass: "kube-vip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := (&DHCPv6Client{}).WithVendorClass(test.enterpriseNumber, test.vendorClass)
			client.WithHostName("nginx-dhcp")

			solicit, err := dhcpv6.NewSolicit(hwaddr, client.requestOptions()...)
			if err != nil {
				t.Fatalf("building Solicit: %v", err)
			}
			advertise, err := dhcpv6.NewAdvertiseFromSolicit(solicit, dhcpv6.WithServerID(serverID), dhcpv6.WithIANA(leasedIP))
			if err != nil {
				t.Fatalf("building Advertise: %v", err)
			}
			request, err := dhcpv6.NewRequestFromAdvertise(advertise, client.requestOptions()...)
			if err != nil {
				t.Fatalf("building Request: %v", err)
			}
			// request builds the Request a second time from the first one, which
			// copies the vendor class of the first Request into the second.
			request.MessageType = dhcpv6.MessageTypeAdvertise
			request, err = dhcpv6.NewRequestFromAdvertise(request, client.requestOptions()...)
			if err != nil {
				t.Fatalf("rebuilding Request: %v", err)
			}

			for _, message := range []*dhcpv6.Message{solicit, request} {
				assertDHCPv6VendorClass(t, message, test.enterpriseNumber, test.vendorClass)
			}
		})
	}
}

// assertDHCPv6VendorClass checks that the message carries a single Vendor Class
// option with the given enterprise number and vendor class, or none when
// vendorClass is empty.
func assertDHCPv6VendorClass(t *testing.T, message *dhcpv6.Message, enterpriseNumber uint32, vendorClass string) {
	t.Helper()
	got := message.Options.VendorClasses()
	if vendorClass == "" {
		if len(got) != 0 {
			t.Errorf("%s has vendor classes %v, want none", message.MessageType, got)
		}
		return
	}
	if len(got) != 1 {
		t.Fatalf("%s has %d vendor class options, want 1", message.MessageType, len(got))
	}
	if got[0].EnterpriseNumber != enterpriseNumber || len(got[0].Data) != 1 || string(got[0].Data[0]) != vendorClass {
		t.Errorf("%s has vendor class %s, want enterprise number %d with data %q", message.MessageType, got[0], enterpriseNumber, vendorClass)
	}
}
