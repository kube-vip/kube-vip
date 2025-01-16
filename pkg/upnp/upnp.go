package upnp

import (
	"context"
	"strings"

	log "log/slog"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"golang.org/x/sync/errgroup"
)

// The UPNP library is structured a bit funny. To create a forward on a gateway and know
// the external IP of that gateway we need two different instances of the UPNP client
// WANIPv6FirewallControlClient can be empty if the gatway doesn't support the WANIPv6FirewallControl service
type Gateway struct {
	ConnectionClient             ConnectionClient
	WANIPv6FirewallControlClient *internetgateway2.WANIPv6FirewallControl1
}

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
	GetServiceClient() *goupnp.ServiceClient
}

func GetGatewayClients(ctx context.Context) []Gateway {
	clients := GetConnectionClients(ctx)
	gatewayClients := make([]Gateway, len(clients))

	for i := range clients {
		gatewayClients[i] = Gateway{ConnectionClient: clients[i], WANIPv6FirewallControlClient: nil}
		gatewayURL := clients[i].GetServiceClient().Location
		if wanipv6clients, err := internetgateway2.NewWANIPv6FirewallControl1ClientsByURLCtx(ctx, gatewayURL); err == nil {
			gatewayClients[i].WANIPv6FirewallControlClient = wanipv6clients[0]
		} else {
			log.Warn("[UPNP] Unable to find WANIPv6FirewallControl1Clients", "Gateway", gatewayURL, "err", err)
		}
	}
	return gatewayClients
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
		log.Error("[UPNP] Could not finish querying UPNP connection clients", "err", err.Error())
		return routers
	}

	errors := append(ip1Error, ip2Error...)
	errors = append(errors, ppp1Error...)

	for _, e := range errors {
		log.Warn("[UPNP] UPNP Gateway responded with an error while querying WAN Connection Client", "err", e)
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
