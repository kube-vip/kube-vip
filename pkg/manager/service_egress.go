package manager

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	log "log/slog"

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
	defer file.Close()
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

func (sm *Manager) nftablesCheck() error {
	file, err := os.Open("/proc/modules")
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	var queue, ct bool
	for scanner.Scan() {
		line := strings.Fields(scanner.Text())
		switch line[0] {
		case "nft_queue":
			queue = true
		case "nft_ct":
			ct = true
		}
	}

	if !queue || !ct {
		return fmt.Errorf("missing nftables modules -> nft_ct [%t] -> ntf_queue [%t] mangle -> [%t]", ct, queue)
	}
	return nil
}

func getSameFamilyCidr(sourceCidrs, ip string) string { //Todo: not sure how this ever worked
	cidrs := strings.Split(sourceCidrs, ",")
	isV6 := vip.IsIPv6(ip)
	matchingFamily := []string{}
	for _, cidr := range cidrs {
		// Is the ip an IPv6 address
		if isV6 {
			if vip.IsIPv6CIDR(cidr) {
				matchingFamily = append(matchingFamily, cidr)
				selectedCIDR, err := checkCIDR(ip, cidr)
				if err != nil {
					log.Warn("IPv6 CIDR check ", "err", err)
					continue
				}
				if selectedCIDR != "" {
					return selectedCIDR
				}
			}
		} else {
			if vip.IsIPv4CIDR(cidr) {
				matchingFamily = append(matchingFamily, cidr)
				selectedCidr, err := checkCIDR(ip, cidr)
				if err != nil {
					log.Warn("IPv4 CIDR check ", "err", err)
					continue
				}
				if selectedCidr != "" {
					return selectedCidr
				}
			}
		}
	}

	if len(matchingFamily) > 0 {
		// return first CIDR that has at least the same IP family as processed IP address
		// (should be better than just returning first CIDR on the list, I think)
		return matchingFamily[0]
	}

	// return to the default behaviour of setting the CIDR to the first one (or only one)
	return cidrs[0]
}

func checkCIDR(ip, cidr string) (string, error) {
	_, ipnetA, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("failed to parse CIDR [%s]: %w", cidr, err)
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return "", fmt.Errorf("failed to parse IP [%s]", ip)
	}
	if ipnetA.Contains(parsedIP) {
		return cidr, nil
	}

	return "", nil
}

func (sm *Manager) configureEgress(vipIP, podIP, namespace string, annotations map[string]string) error {
	var podCidr, serviceCidr string
	var autoServiceCIDR, autoPodCIDR string
	var discoverErr error

	// Look up the destination ports from the annotations on the service
	destinationPorts := annotations[egressDestinationPorts]
	deniedNetworks := annotations[egressDeniedNetworks]
	allowedNetworks := annotations[egressAllowedNetworks]

	if sm.config.EgressPodCidr == "" || sm.config.EgressServiceCidr == "" {
		autoServiceCIDR, autoPodCIDR, discoverErr = sm.AutoDiscoverCIDRs()
	}

	if discoverErr != nil {
		log.Warn("autodiscover CIDR", "err", discoverErr)
	}

	if sm.config.EgressPodCidr != "" {
		podCidr = getSameFamilyCidr(sm.config.EgressPodCidr, podIP)
	} else {
		if discoverErr == nil {
			podCidr = getSameFamilyCidr(autoPodCIDR, podIP)
		}
	}

	if podCidr == "" {
		// There's no default IPv6 pod CIDR, therefore we silently back off if CIDR s not specified.
		if !vip.IsIPv4(podIP) {
			return fmt.Errorf("error with the CIDR [%s]", podIP)
		}
		podCidr = defaultPodCIDR
	}

	if sm.config.EgressServiceCidr != "" {
		serviceCidr = getSameFamilyCidr(sm.config.EgressServiceCidr, vipIP)
	} else {
		if discoverErr == nil {
			serviceCidr = getSameFamilyCidr(autoServiceCIDR, vipIP)
		}
	}

	if serviceCidr == "" {
		// There's no default IPv6 service CIDR, therefore we silently back off if CIDR s not specified.
		if !vip.IsIPv4(vipIP) {
			return nil
		}
		serviceCidr = defaultServiceCIDR
	}

	log.Info("[Egress]", "podCIDR", podCidr, "serviceCIDR", serviceCidr, "vip", serviceCidr, "pod", podIP)

	// checking if all addresses are of the same IP family
	if vip.IsIPv4(podIP) != vip.IsIPv4CIDR(podCidr) {
		log.Error("[Egress] family is not matching. Backing off...", "pod", podIP, "podCIDR", podCidr)
		return nil
	}

	if vip.IsIPv4(vipIP) != vip.IsIPv4CIDR(serviceCidr) {
		log.Error("[Egress] family is not matching. Backing off...", "pod", podIP, "serviceCIDR", serviceCidr)
		return nil
	}

	if vip.IsIPv4(vipIP) != vip.IsIPv4(podIP) {
		log.Error("[Egress] family is not matching. Backing off...", "pod", podIP, "vipIP", vipIP)
		return nil
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

	if deniedNetworks != "" {
		networks := strings.Split(deniedNetworks, ",")
		for x := range networks {
			err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, networks[x])
			if err != nil {
				return fmt.Errorf("error adding rules to mangle chain [%s], error [%s]", vip.MangleChainName, err)
			}
		}
	}

	mask := "/32"
	if !vip.IsIPv4(podIP) {
		mask = "/128"
	}

	if allowedNetworks != "" {
		networks := strings.Split(allowedNetworks, ",")
		for x := range networks {
			err = i.AppendReturnRulesForMarkingForNetwork(vip.MangleChainName, podIP+mask, networks[x])
			if err != nil {
				return fmt.Errorf("error adding marking rules to mangle chain [%s], error [%s]", vip.MangleChainName, err)
			}
		}
	} else {
		err = i.AppendReturnRulesForMarking(vip.MangleChainName, podIP+mask)
		if err != nil {
			return fmt.Errorf("error adding marking rules to mangle chain [%s], error [%s]", vip.MangleChainName, err)
		}
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

	err = vip.DeleteExistingSessions(podIP, false, destinationPorts, "")
	if err != nil {
		return err
	}

	return nil
}

func (sm *Manager) AutoDiscoverCIDRs() (serviceCIDR, podCIDR string, err error) {
	log.Debug("Trying to automatically discover Service and Pod CIDRs")
	options := v1.ListOptions{
		LabelSelector: "component=kube-controller-manager",
	}
	podList, err := sm.clientSet.CoreV1().Pods("kube-system").List(context.TODO(), options)
	if err != nil {
		return "", "", fmt.Errorf("[Egress] Unable to get kube-controller-manager pod: %w", err)
	}
	if len(podList.Items) < 1 {
		return "", "", fmt.Errorf("[Egress] Unable to auto-discover the pod/service CIDRs: kube-controller-manager not found")
	}

	pod := podList.Items[0]
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

func (sm *Manager) TeardownEgress(podIP, vipIP, namespace string, annotations map[string]string) error {
	// Look up the destination ports from the annotations on the service
	destinationPorts := annotations[egressDestinationPorts]
	deniedNetworks := annotations[egressDeniedNetworks]
	allowedNetworks := annotations[egressAllowedNetworks]

	protocol := iptables.ProtocolIPv4
	if vip.IsIPv6(podIP) {
		protocol = iptables.ProtocolIPv6
	}

	i, err := vip.CreateIptablesClient(sm.config.EgressWithNftables, namespace, protocol)
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
