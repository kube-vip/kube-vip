package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/vip"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (sm *Manager) configureEgress(vipIP, podIP string) error {
	serviceCIDR, podCIDR, err := sm.AutoDiscoverCIDRs()
	if err != nil {
		serviceCIDR = "10.96.0.0/12"
		podCIDR = "10.0.0.0/16"
	}
	i, err := vip.CreateIptablesClient()
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
	err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, podCIDR)
	if err != nil {
		panic(err)
	}
	err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, serviceCIDR)
	if err != nil {
		panic(err)
	}
	err = i.AppendReturnRulesForMarking(vip.MangleChainName, podIP+"/32")
	if err != nil {
		panic(err)
	}

	err = i.InsertMangeTableIntoPrerouting(vip.MangleChainName)
	if err != nil {
		panic(err)
	}
	err = i.InsertSourceNat(vipIP, podIP)
	if err != nil {
		panic(err)
	}

	_ = i.DumpChain(vip.MangleChainName)
	err = vip.DeleteExistingSessions(podIP)
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

func TeardownEgress(podIP, serviceIP string) error {
	i, err := vip.CreateIptablesClient()
	if err != nil {
		return fmt.Errorf("error Creating iptables client [%s]", err)
	}
	return i.DeleteSourceNat(podIP, serviceIP)
}
