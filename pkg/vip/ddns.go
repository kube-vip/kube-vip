package vip

import (
	"context"
	"net"
	"time"

	dhclient "github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// DDNSManager will start a dhclient to retrieve and keep the lease for the IP
// for the dDNSHostName
// will return the IP allocated
type DDNSManager interface {
	Start() (string, error)
}

type ddnsManager struct {
	ctx     context.Context
	network Network
}

func NewDDNSManager(ctx context.Context, network Network) DDNSManager {
	return &ddnsManager{
		ctx:     ctx,
		network: network,
	}
}

// Start will start the dhcpclient routine to keep the lease
// and return the IP it got from DHCP
func (ddns *ddnsManager) Start() (string, error) {
	interfaceName := ddns.network.Interface()
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return "", err
	}

	// channel to wait for IP
	ipCh := make(chan string)

	client := dhclient.Client{
		// send host name, not current os.Hostname, but configured name from user
		Hostname: ddns.network.DDNSHostName(),
		Iface:    iface,
		OnBound: func(lease *dhclient.Lease) {
			ipCh <- lease.FixedAddress.String()
		},
	}

	// add default request param
	for _, param := range dhclient.DefaultParamsRequestList {
		client.AddParamRequest(param)
	}
	// send hostname to get IP for this specific name
	client.AddOption(layers.DHCPOptHostname, []byte(client.Hostname))
	// use clientID so different leaders will use same ID to get same IP
	client.AddOption(layers.DHCPOptClientID, []byte(client.Hostname))

	log.Info("start dhcp client for ddns")
	client.Start()

	log.Info("waiting for ip from dhcp")
	ip, timeout := "", time.After(1*time.Minute)

	select {
	case <-ddns.ctx.Done():
		client.Stop()
		return "", errors.New("context cancelled")
	case <-timeout:
		client.Stop()
		return "", errors.New("failed to get IP from dhcp for ddns in 1 minutes")
	case ip = <-ipCh:
		log.Info("got ip from dhcp: ", ip)
	}

	// lease.FixedAddress.String() could return <nil>
	if ip == "<nil>" {
		return "", errors.New("failed to get IP from dhcp for ddns, got ip as <nil>")
	}

	// start a go routine to stop dhclient when lose leader election
	// also to keep read the ip from channel
	// so onbound function is unblocked to send the ip
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				log.Info("stop dhclient for ddns")
				client.Stop()
				return
			case ip := <-ipCh:
				log.Info("got ip from dhcp: ", ip)
			}
		}
	}(ddns.ctx)

	return ip, nil
}
