package egress

import (
	"fmt"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

func Teardown(podIP, vipIP, namespace, serviceUUID string, annotations map[string]string, useNftables bool) error {
	// Look up the destination ports from the annotations on the service
	destinationPorts := annotations[kubevip.EgressDestinationPorts]
	deniedNetworks := annotations[kubevip.EgressDeniedNetworks]
	allowedNetworks := annotations[kubevip.EgressAllowedNetworks]
	internalEgress := annotations[kubevip.EgressInternal]

	protocol := iptables.ProtocolIPv4
	IPv6 := false
	if utils.IsIPv6(podIP) {
		protocol = iptables.ProtocolIPv6
		IPv6 = true
	}

	// Use the internal egress implementation
	if internalEgress != "" {
		return nftables.DeleteSNAT(IPv6, serviceUUID)
	}

	i, err := vip.CreateIptablesClient(useNftables, namespace, protocol)
	if err != nil {
		return fmt.Errorf("error Creating iptables client [%s]", err)
	}

	if deniedNetworks != "" {
		networks := strings.Split(deniedNetworks, ",")
		for x := range networks {
			err = i.DeleteMangleReturnForNetwork(vip.MangleChainName, networks[x])
			if err != nil {
				return fmt.Errorf("error deleting rules in mangle chain [%s], error [%s]", vip.MangleChainName, err)
			}
		}
	}

	if allowedNetworks != "" {
		networks := strings.Split(allowedNetworks, ",")
		for x := range networks {
			err = i.DeleteMangleMarkingForNetwork(podIP, vip.MangleChainName, networks[x])
			if err != nil {
				return fmt.Errorf("error deleting rules in mangle chain [%s], error [%s]", vip.MangleChainName, err)
			}
		}
	} else {
		// Remove the marking of egress packets
		err = i.DeleteMangleMarking(podIP, vip.MangleChainName)
		if err != nil {
			return fmt.Errorf("error changing iptables rules for egress [%s]", err)
		}
	}

	// Clear up SNAT rules
	if destinationPorts != "" {
		fixedPorts := strings.Split(destinationPorts, ",")

		for _, fixedPort := range fixedPorts {
			var proto, port string

			data := strings.Split(fixedPort, ":")
			if len(data) == 0 {
				continue
			} else if len(data) == 1 {
				proto = "tcp"
				port = data[0]
			} else {
				proto = data[0]
				port = data[1]
			}

			err = i.DeleteSourceNatForDestinationPort(podIP, vipIP, port, proto)
			if err != nil {
				return fmt.Errorf("error changing iptables rules for egress [%s]", err)
			}

		}
	} else {
		err = i.DeleteSourceNat(podIP, vipIP)
		if err != nil {
			return fmt.Errorf("error changing iptables rules for egress [%s]", err)
		}
	}

	err = vip.DeleteExistingSessions(podIP, false, destinationPorts, "")
	if err != nil {
		return fmt.Errorf("error changing iptables rules for egress [%s]", err)
	}
	return nil
}
