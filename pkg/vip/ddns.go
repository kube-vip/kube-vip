package vip

import (
	"context"
	"net"
	"time"

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

// NewDDNSManager returns a newly created Dynamic DNS manager
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

	client := NewDHCPClient(iface, false, "")

	client.WithHostName(ddns.network.DDNSHostName())

	go client.Start()

	log.Info("waiting for ip from dhcp")
	ip, timeout := "", time.After(1*time.Minute)

	select {
	case <-ddns.ctx.Done():
		client.Stop()
		return "", errors.New("context cancelled")
	case <-timeout:
		client.Stop()
		return "", errors.New("failed to get IP from dhcp for ddns in 1 minutes")
	case ip = <-client.IPChannel():
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
			case ip := <-client.IPChannel():
				log.Info("got ip from dhcp: ", ip)
			}
		}
	}(ddns.ctx)

	return ip, nil
}
