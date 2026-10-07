package vip

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/utils"
)

// channelClosed reports whether ch has already been closed without blocking.
// Callers use it while holding their lifecycle mutex when they may close ch.
func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type DHCPClient interface {
	ErrorChannel() chan error
	IPChannel() chan string
	Start(ctx context.Context) error
	Stop()
	WithHostName(hostname string) DHCPClient
}

func NewDHCPClient(network Network, backoffAttempts uint) (DHCPClient, error) {
	interfaceName := network.Interface()
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to get interface %q: %w", interfaceName, err)
	}

	var client DHCPClient

	if strings.EqualFold(network.DHCPFamily(), utils.IPv6Family) {
		client, err = NewDHCPv6Client(iface, nil, false, "", backoffAttempts)
		if err != nil {
			return nil, fmt.Errorf("failed to create DHCP client: %w", err)
		}
	} else {
		client = NewDHCPv4Client(iface, false, "", backoffAttempts, false)
	}

	return client, nil
}
