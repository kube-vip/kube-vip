package loadbalancer

import (
	"fmt"
	"net"
	"strings"

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
	forwardingMethod    ipvs.ForwardType
}

func NewIPVSLB(address string, port int, forwardingMethod string) (*IPVSLoadBalancer, error) {

	// Create IPVS client
	c, err := ipvs.New()
	if err != nil {
		log.Errorf("ensure IPVS kernel modules are loaded")
		log.Fatalf("Error starting IPVS [%v]", err)
	}
	i, err := c.Info()
	if err != nil {
		log.Errorf("ensure IPVS kernel modules are loaded")
		log.Fatalf("Error getting IPVS version [%v]", err)
	}
	log.Infof("IPVS Loadbalancer enabled for %d.%d.%d", i.Version[0], i.Version[1], i.Version[2])

	// Generate out API Server LoadBalancer instance
	svc := ipvs.Service{
		Family:    ipvs.INET,
		Protocol:  ipvs.TCP,
		Port:      uint16(port),
		Address:   ipvs.NewIP(net.ParseIP(address)),
		Scheduler: ROUNDROBIN,
	}

	var m ipvs.ForwardType
	switch strings.ToLower(forwardingMethod) {
	case "masquerade":
		m = ipvs.Masquarade
	case "local":
		m = ipvs.Local
	case "tunnel":
		m = ipvs.Tunnel
	case "directroute":
		m = ipvs.DirectRoute
	case "bypass":
		m = ipvs.Bypass
	default:
		m = ipvs.Local
		log.Warnf("unknown forwarding method. Defaulting to Local")
	}

	lb := &IPVSLoadBalancer{
		Port:                port,
		client:              c,
		loadBalancerService: svc,
		forwardingMethod:    m,
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

func (lb *IPVSLoadBalancer) AddBackend(address string, port int) error {

	// Check if this is the first backend
	backends, err := lb.client.Destinations(lb.loadBalancerService)
	if err != nil && strings.Contains(err.Error(), "file does not exist") {
		log.Errorf("Error querying backends %s", err)
	}
	// If this is our first backend, then we can create the load-balancer service and add a backend
	if len(backends) == 0 {
		err = lb.client.CreateService(lb.loadBalancerService)
		// If we've an error it could be that the IPVS lb instance has been left from a previous leadership
		if err != nil && strings.Contains(err.Error(), "file exists") {
			log.Warnf("load balancer for API server already exists, attempting to remove and re-create")
			err = lb.client.RemoveService(lb.loadBalancerService)

			if err != nil {
				return fmt.Errorf("error re-creating IPVS service: %v", err)
			}
			err = lb.client.CreateService(lb.loadBalancerService)
			if err != nil {
				return fmt.Errorf("error re-creating IPVS service: %v", err)
			}
		} else if err != nil {
			// Fatal error at this point as IPVS is probably not working
			log.Errorf("Unable to create an IPVS service, ensure IPVS kernel modules are loaded")
			log.Fatalf("IPVS service error: %v", err)
		}
		log.Infof("Created Load-Balancer services on [%s:%d]", lb.loadBalancerService.Address.Net(ipvs.INET).String(), lb.Port)
	}

	dst := ipvs.Destination{
		Address:   ipvs.NewIP(net.ParseIP(address)),
		Port:      uint16(port),
		Family:    ipvs.INET,
		Weight:    1,
		FwdMethod: lb.forwardingMethod,
	}

	err = lb.client.CreateDestination(lb.loadBalancerService, dst)
	// Swallow error of existing back end, the node watcher may attempt to apply
	// the same back end multiple times
	if err != nil {
		if !strings.Contains(err.Error(), "file exists") {
			return fmt.Errorf("error creating backend: %v", err)
		}
		// file exists is fine, we will just return at this point
		return nil
	}
	log.Infof("Added backend for [%s:%d] on [%s:%d]", lb.loadBalancerService.Address.Net(ipvs.INET).String(), lb.Port, address, port)

	return nil
}

func (lb *IPVSLoadBalancer) RemoveBackend(address string, port int) error {
	dst := ipvs.Destination{
		Address: ipvs.NewIP(net.ParseIP(address)),
		Port:    uint16(port),
		Family:  ipvs.INET,
		Weight:  1,
	}
	err := lb.client.RemoveDestination(lb.loadBalancerService, dst)
	if err != nil {
		return fmt.Errorf("error removing backend: %v", err)
	}
	return nil
}
