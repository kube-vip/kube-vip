package manager

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/vip"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DEBUG
const (
	defaultPodCIDR     = "10.0.0.0/16"
	defaultServiceCIDR = "10.96.0.0/12"
)

func (sm *Manager) iptablesCheck() error {
	file, err := os.Open("/proc/modules")
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	var nat, filter, mangle bool
	for scanner.Scan() {
		line := strings.Fields(scanner.Text())
		switch line[0] {
		case "iptable_filter":
			filter = true
		case "iptable_nat":
			nat = true
		case "iptable_mangle":
			mangle = true
		}
	}

	if !filter || !nat || !mangle {
		return fmt.Errorf("missing iptables modules -> nat [%t] -> filter [%t] mangle -> [%t]", nat, filter, mangle)
	}
	return nil
}

func getSameFamilyCidr(source, ip string) string {
	cidrs := strings.Split(source, ",")
	for _, cidr := range cidrs {
		source = cidr
		if vip.IsIPv4(cidr) == vip.IsIPv4(ip) {
			return cidr
		}
	}
	// return to the default behaviour of setting the CIDR to the first one (or only one)
	return source
}

func (sm *Manager) configureEgress(vipIP, podIP, destinationPorts, namespace string) error {
	// serviceCIDR, podCIDR, err := sm.AutoDiscoverCIDRs()
	// if err != nil {
	// 	serviceCIDR = "10.96.0.0/12"
	// 	podCIDR = "10.0.0.0/16"
	// }

	var podCidr, serviceCidr string

	if sm.config.EgressPodCidr != "" {
		podCidr = getSameFamilyCidr(sm.config.EgressPodCidr, podIP)
	} else {
		// There's no default IPv6 pod CIDR, therefore we silently back off if CIDR s not specified.
		if !vip.IsIPv4(podIP) {
			return nil
		}
		podCidr = defaultPodCIDR
	}

	if sm.config.EgressServiceCidr != "" {
		serviceCidr = getSameFamilyCidr(sm.config.EgressServiceCidr, vipIP)
	} else {
		// There's no default IPv6 service CIDR, therefore we silently back off if CIDR s not specified.
		if !vip.IsIPv4(vipIP) {
			return nil
		}
		serviceCidr = defaultServiceCIDR
	}

	protocol := iptables.ProtocolIPv4

	if vip.IsIPv6(vipIP) {
		protocol = iptables.ProtocolIPv6
	}

	i, err := vip.CreateIptablesClient(sm.config.EgressWithNftables, namespace, protocol)
	if err != nil {
		return fmt.Errorf("error Creating iptables client [%s]", err)
	}

	// Check if the kube-vip mangle chain exists, if not create it
	exists, err := i.CheckMangleChain(vip.MangleChainName)
	if err != nil {
		return fmt.Errorf("error checking for existence of mangle chain [%s], error [%s]", vip.MangleChainName, err)
	}
	if !exists {
		err = i.CreateMangleChain(vip.MangleChainName)
		if err != nil {
			return fmt.Errorf("error creating mangle chain [%s], error [%s]", vip.MangleChainName, err)
		}
	}
	err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, podCidr)
	if err != nil {
		return fmt.Errorf("error adding rules to mangle chain [%s], error [%s]", vip.MangleChainName, err)
	}
	err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, serviceCidr)
	if err != nil {
		return fmt.Errorf("error adding rules to mangle chain [%s], error [%s]", vip.MangleChainName, err)
	}

	mask := "/32"
	if !vip.IsIPv4(podIP) {
		mask = "/128"
	}

	err = i.AppendReturnRulesForMarking(vip.MangleChainName, podIP+mask)
	if err != nil {
		return fmt.Errorf("error adding marking rules to mangle chain [%s], error [%s]", vip.MangleChainName, err)
	}

	err = i.InsertMangeTableIntoPrerouting(vip.MangleChainName)
	if err != nil {
		return fmt.Errorf("error adding prerouting mangle chain [%s], error [%s]", vip.MangleChainName, err)
	}

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

			err = i.InsertSourceNatForDestinationPort(vipIP, podIP, port, proto)
			if err != nil {
				return fmt.Errorf("error adding snat rules to nat chain [%s], error [%s]", vip.MangleChainName, err)
			}

		}
	} else {
		err = i.InsertSourceNat(vipIP, podIP)
		if err != nil {
			return fmt.Errorf("error adding snat rules to nat chain [%s], error [%s]", vip.MangleChainName, err)
		}
	}
	//_ = i.DumpChain(vip.MangleChainName)
	err = vip.DeleteExistingSessions(podIP, false)
	if err != nil {
		return err
	}

	return nil
}

func (sm *Manager) AutoDiscoverCIDRs() (serviceCIDR, podCIDR string, err error) {
	pod, err := sm.clientSet.CoreV1().Pods("kube-system").Get(context.TODO(), "kube-controller-manager", v1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	for flags := range pod.Spec.Containers[0].Command {
		if strings.Contains(pod.Spec.Containers[0].Command[flags], "--cluster-cidr=") {
			podCIDR = strings.ReplaceAll(pod.Spec.Containers[0].Command[flags], "--cluster-cidr=", "")
		}
		if strings.Contains(pod.Spec.Containers[0].Command[flags], "--service-cluster-ip-range=") {
			serviceCIDR = strings.ReplaceAll(pod.Spec.Containers[0].Command[flags], "--service-cluster-ip-range=", "")
		}
	}
	if podCIDR == "" || serviceCIDR == "" {
		err = fmt.Errorf("unable to fully determine cluster CIDR configurations")
	}

	return
}

func (sm *Manager) TeardownEgress(podIP, vipIP, destinationPorts, namespace string) error {
	protocol := iptables.ProtocolIPv4
	if vip.IsIPv6(podIP) {
		protocol = iptables.ProtocolIPv6
	}

	i, err := vip.CreateIptablesClient(sm.config.EgressWithNftables, namespace, protocol)
	if err != nil {
		return fmt.Errorf("error Creating iptables client [%s]", err)
	}

	// Remove the marking of egress packets
	err = i.DeleteMangleMarking(podIP, vip.MangleChainName)
	if err != nil {
		return fmt.Errorf("error changing iptables rules for egress [%s]", err)
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
	err = vip.DeleteExistingSessions(podIP, false)
	if err != nil {
		return fmt.Errorf("error changing iptables rules for egress [%s]", err)
	}
	return nil
}
