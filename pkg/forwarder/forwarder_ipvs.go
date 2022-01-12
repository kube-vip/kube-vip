package forwarder

import (
	log "github.com/sirupsen/logrus"

	"github.com/kube-vip/kube-vip/pkg/loadbalancer"
)

func startIPVS(srcAddress string, srcPort int, destAddress string, destPort int, forwardingMethod string) error {
	log.Infof("Setting up IPVS backend forwarding: from %s:%d to %s:%d using method %s", srcAddress, srcPort, destAddress, destPort, forwardingMethod)
	lb, err := loadbalancer.NewIPVSLB(srcAddress, srcPort, forwardingMethod)
	if err != nil {
		return err
	}
	return lb.AddBackend(destAddress, destPort)
}

func stopIPVS(srcAddress string, srcPort int, destAddress string, destPort int, forwardingMethod string) error {
	log.Infof("Stopping IPVS backend forwarding: from %s:%d to %s:%d using method %s", srcAddress, srcPort, destAddress, destPort, forwardingMethod)
	lb, err := loadbalancer.NewIPVSLB(srcAddress, srcPort, forwardingMethod)
	if err != nil {
		return err
	}
	return lb.RemoveIPVSLB()
}
