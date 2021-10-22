package loadbalancer

import (
	"fmt"
	"net"
	"os"

	"github.com/cloudflare/ipvs"
	log "github.com/sirupsen/logrus"
)

/*
IPVS Architecture - for those that are interested

There are going to be a large number of end users that are using a VIP that exists within the same subnet
as the back end servers. This unfortunately will result in "packet" confusion with the destingation and
source becoming messed up by the IPVS NAT.

The solution is to perform two things !

First:
Set up kube-vip TCP port forwarder from the VIP:PORT to the IPVS:PORT

Second:
Start up a node watcher and a IPVS load balancer, the node balancer is responsible for adding/removing
the nodes from the IPVS load-balancer.

*/

const (
	ROUNDROBIN = "rr"
)

type IPVSLoadBalancer struct {
	client              ipvs.Client
	loadBalancerService ipvs.Service
	Port                int
}

func NewIPVSLB(address string, port int) (*IPVSLoadBalancer, error) {

	// Create IPVS client
	c, err := ipvs.New()
	if err != nil {
		return nil, fmt.Errorf("error creating IPVS client: %v", err)
	}

	// Generate out API Server LoadBalancer instance
	svc := ipvs.Service{
		Family:    ipvs.INET,
		Protocol:  ipvs.TCP,
		Port:      uint16(port),
		Address:   ipvs.NewIP(net.ParseIP(address)),
		Scheduler: ROUNDROBIN,
	}
	err = c.CreateService(svc)
	// If we've an error it could be that the IPVS lb instance has been left from a previous leadership
	if err == os.ErrExist {
		log.Warnf("load balancer for API server already exists, attempting to remove and re-create")

		err = c.CreateService(svc)
		if err != nil {
			return nil, fmt.Errorf("error re-creating IPVS service: %v", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("error creating IPVS service: %v", err)
	}

	lb := &IPVSLoadBalancer{
		Port:                port,
		client:              c,
		loadBalancerService: svc,
	}
	// Return our created load-balancer
	return lb, nil
}

func (lb *IPVSLoadBalancer) RemoveIPVSLB() error {
	err := lb.client.RemoveService(lb.loadBalancerService)
	if err != nil {
		return fmt.Errorf("error removing existing IPVS service: %v", err)
	}
	return nil

}

func (lb *IPVSLoadBalancer) AddBackend(address string) error {
	dst := ipvs.Destination{
		Address: ipvs.NewIP(net.ParseIP(address)),
		Port:    6443,
		Family:  ipvs.INET,
		Weight:  1,
	}
	err := lb.client.CreateDestination(lb.loadBalancerService, dst)
	// TODO: we either parse the IPVS configuration to see if a backend exists
	// or if it already exists we just ignore this and continue ons
	if err != nil && err != os.ErrExist {
		return fmt.Errorf("error creating backend: %v", err)
	}
	return nil
}

func (lb *IPVSLoadBalancer) RemoveBackend(address string) error {
	dst := ipvs.Destination{
		Address: ipvs.NewIP(net.ParseIP(address)),
		Port:    6443,
		Family:  ipvs.INET,
		Weight:  1,
	}
	err := lb.client.RemoveDestination(lb.loadBalancerService, dst)
	if err != nil {
		return fmt.Errorf("error removing backend: %v", err)
	}
	return nil
}
