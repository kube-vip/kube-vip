package upnp

import (
	"context"
	"strings"

	"github.com/huin/goupnp/dcps/internetgateway2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

type ConnectionClient interface {
	AddPortMapping(
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
	) (err error)

	GetExternalIPAddress() (
		NewExternalIPAddress string,
		err error,
	)
}

// Gather WANIPv6FirewallControl1 clients in the network and treats errors as non-critical.
// Use this to create Pinholes in the UPNP IGD2
func GetWANIPv6FirewallControl1ClientsCtx(ctx context.Context) []internetgateway2.WANIPv6FirewallControl1 {
	c, errors, err := internetgateway2.NewWANIPv6FirewallControl1ClientsCtx(ctx)
	var validClients []internetgateway2.WANIPv6FirewallControl1
	if err != nil {
		log.Warnf("Unable to query for WAN Firewall Clients [%s]", err.Error())
	}
	for _, e := range errors {
		log.Warnf("UPNP Gateway responded with an error while querying WAN Firewall Client [%s]", e.Error())
	}
	for _, c := range c {
		if c != nil {
			validClients = append(validClients, *c)
		}
	}
	return validClients
}

// Gather UPNPConnectionClients in the network and treats errors as non-critical. Use this to find out the external IPs of the network and configure port forwarding
func GetConnectionClients(ctx context.Context) []ConnectionClient {
	tasks, _ := errgroup.WithContext(ctx)
	// This code sucks and I did not manage to make it more concise.
	//Turns out using []*type instead of []type prevents you from casting to an interface.
	// If you have any better ideas please take a stab at it.
	var ip1Clients []*internetgateway2.WANIPConnection1
	var ip1Error []error
	tasks.Go(func() error {
		var err error
		ip1Clients, ip1Error, err = internetgateway2.NewWANIPConnection1Clients()
		return err
	})
	var ip2Clients []*internetgateway2.WANIPConnection2
	var ip2Error []error
	tasks.Go(func() error {
		var err error
		ip2Clients, ip2Error, err = internetgateway2.NewWANIPConnection2Clients()
		return err
	})
	var ppp1Clients []*internetgateway2.WANPPPConnection1
	var ppp1Error []error
	tasks.Go(func() error {
		var err error
		ppp1Clients, ppp1Error, err = internetgateway2.NewWANPPPConnection1Clients()
		return err
	})

	var routers []ConnectionClient

	if err := tasks.Wait(); err != nil {
		log.Errorf("Could not finish querying UPNP connection clients [%s]", err.Error())
		return routers
	}

	errors := append(ip1Error, ip2Error...)
	errors = append(errors, ppp1Error...)

	for _, e := range errors {
		log.Warnf("UPNP Gateway responded with an error while querying WAN Connection Client [%s]", e.Error())
	}

	for _, c := range ip1Clients {
		if c != nil {
			routers = append(routers, c)
		}
	}

	for _, c := range ip2Clients {
		if c != nil {
			routers = append(routers, c)
		}
	}

	for _, c := range ppp1Clients {
		if c != nil {
			routers = append(routers, c)
		}
	}
	return routers
}

func MapProtocolToIANA(p string) uint16 {
	switch strings.ToUpper(p) {
	case "TCP":
		return 6
	case "UDP":
		return 17
	default:
		return 0
	}
}
