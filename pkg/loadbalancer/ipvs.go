package loadbalancer

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"

	log "log/slog"

	"github.com/cloudflare/ipvs"
	"github.com/cloudflare/ipvs/netmask"
	"github.com/kube-vip/kube-vip/pkg/backend"
	"github.com/kube-vip/kube-vip/pkg/sysctl"
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
	Port                uint16
	forwardingMethod    ipvs.ForwardType
	backendMap          backend.Map
	interval            int
	lock                sync.Mutex
	stop                chan struct{}
}

func NewIPVSLB(address string, port uint16, forwardingMethod string, backendHealthCheckInterval int) (*IPVSLoadBalancer, error) {
	// Create IPVS client
	c, err := ipvs.New()
	if err != nil {
		log.Error("ensure IPVS kernel modules are loaded")
		log.Error("Error starting IPVS", "err", err)
		panic("")
	}
	i, err := c.Info()
	if err != nil {
		log.Error("ensure IPVS kernel modules are loaded")
		log.Error("Error retrieving IPVS info", "err", err)
		panic("")
	}
	log.Info("IPVS Loadbalancer enabled", "version", fmt.Sprintf(" %d.%d.%d", i.Version[0], i.Version[1], i.Version[2]))

	if strings.ToLower(forwardingMethod) == "masquerade" {
		err = sysctl.WriteProcSys("/proc/sys/net/ipv4/vs/conntrack", "1")
		if err != nil {
			log.Error("ensuring net.ipv4.vs.conntrack enabled", "err", err)
			panic("")
		}
		log.Info("sysctl set net.ipv4.vs.conntrack to 1")

		err = sysctl.WriteProcSys("/proc/sys/net/ipv4/ip_forward", "1")
		if err != nil {
			log.Error("ensuring net.ipv4.ip_forward enabled", "err", err)
			panic("")
		}
		log.Info("sysctl set net.ipv4.ip_forward to 1")
	}

	ip, family := ipAndFamily(address)

	netMask := netmask.MaskFrom(31, 32) // For ipv4
	if family == ipvs.INET6 {
		netMask = netmask.MaskFrom(128, 128) // For ipv6
	}

	// Generate out API Server LoadBalancer instance
	svc := ipvs.Service{
		Netmask:   netMask,
		Family:    family,
		Protocol:  ipvs.TCP,
		Port:      port,
		Address:   ip,
		Scheduler: ROUNDROBIN,
	}

	var m ipvs.ForwardType
	switch strings.ToLower(forwardingMethod) {
	case "masquerade":
		m = ipvs.Masquerade
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
		log.Warn("unknown forwarding method. Defaulting to Local")
	}

	if backendHealthCheckInterval <= 0 {
		backendHealthCheckInterval = 5
	}

	lb := &IPVSLoadBalancer{
		Port:                port,
		client:              c,
		loadBalancerService: svc,
		forwardingMethod:    m,
		interval:            backendHealthCheckInterval,
		backendMap:          make(backend.Map),
	}

	if strings.ToLower(forwardingMethod) == "masquerade" {
		go lb.healthCheck()
	}

	// Return our created load-balancer
	return lb, nil
}

func (lb *IPVSLoadBalancer) RemoveIPVSLB() error {
	close(lb.stop)
	err := lb.client.RemoveService(lb.loadBalancerService)
	if err != nil {
		return fmt.Errorf("error removing existing IPVS service: %v", err)
	}
	return nil
}

func (lb *IPVSLoadBalancer) AddBackend(address string, port uint16) error {
	backend := backend.Entry{Addr: address, Port: port}

	lb.lock.Lock()
	defer lb.lock.Unlock()
	if _, ok := lb.backendMap[backend]; !ok {
		isHealth := backend.Check()
		if isHealth {
			err := lb.addBackend(backend)
			if err != nil {
				return err
			}
		}
		lb.backendMap[backend] = isHealth
	}
	return nil
}

func (lb *IPVSLoadBalancer) addBackend(backend backend.Entry) error {
	// Check if this is the first backend
	backends, err := lb.client.Destinations(lb.loadBalancerService)
	if err != nil && strings.Contains(err.Error(), "file does not exist") {
		log.Error("querying backends", "err", err)
	}
	// If this is our first backend, then we can create the load-balancer service and add a backend
	if len(backends) == 0 {
		err = lb.client.CreateService(lb.loadBalancerService)
		// If we've an error it could be that the IPVS lb instance has been left from a previous leadership
		if err != nil && strings.Contains(err.Error(), "file exists") {
			log.Warn("load balancer for API server already exists, attempting to remove and re-create")
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
			log.Error("Unable to create an IPVS service, ensure IPVS kernel modules are loaded")
			log.Error("IPVS service", "err", err)
			panic("")

		}
		log.Info("load-Balancer services created", "address", lb.addrString(), "port", lb.Port)
	}

	ip, family := ipAndFamily(backend.Addr)

	// Ignore backends that use a different address family.
	// Looks like different families could be supported in tunnel mode...
	if family != lb.loadBalancerService.Family {
		return nil
	}

	dst := ipvs.Destination{
		Address:   ip,
		Port:      backend.Port,
		Family:    family,
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
	log.Info("backend added", "src addr", lb.addrString(), "src port", lb.Port, "dst addr", backend.Addr, "dst port", backend.Port)

	return nil
}

func (lb *IPVSLoadBalancer) RemoveBackend(address string, port uint16) error {
	backend := backend.Entry{Addr: address, Port: port}

	lb.lock.Lock()
	defer lb.lock.Unlock()

	if _, ok := lb.backendMap[backend]; ok {
		err := lb.removeBackend(address, port)
		if err != nil {
			return err
		}
		delete(lb.backendMap, backend)
	}

	return nil
}

func (lb *IPVSLoadBalancer) removeBackend(address string, port uint16) error {
	ip, family := ipAndFamily(address)
	if family != lb.loadBalancerService.Family {
		return nil
	}

	dst := ipvs.Destination{
		Address: ip,
		Port:    port,
		Family:  family,
		Weight:  1,
	}
	err := lb.client.RemoveDestination(lb.loadBalancerService, dst)
	if err != nil {
		return fmt.Errorf("error removing backend: %v", err)
	}
	return nil
}

func (lb *IPVSLoadBalancer) addrString() string {
	return lb.loadBalancerService.Address.String()
}

func ipAndFamily(address string) (netip.Addr, ipvs.AddressFamily) {
	ipAddr := net.ParseIP(address)
	if ipAddr.To4() == nil {
		return netip.AddrFrom16([16]byte(ipAddr.To16())), ipvs.INET6
	}
	return netip.AddrFrom4([4]byte(ipAddr.To4())), ipvs.INET
}

func (lb *IPVSLoadBalancer) healthCheck() {
	backend.Watch(func() {
		lb.lock.Lock()
		for backend, oldStatus := range lb.backendMap {
			newStatus := backend.Check()
			if newStatus {
				// old status -> health
				if !oldStatus {
					err := lb.AddBackend(backend.Addr, backend.Port)
					if err != nil {
						log.Error("add backend", "err", err)
					}
					lb.backendMap[backend] = newStatus
				}
			} else {
				// old status -> not health
				if oldStatus {
					log.Info("healthCheck failed removing backend", "address", backend.Addr, "port", backend.Port)
					err := lb.removeBackend(backend.Addr, backend.Port)
					if err != nil {
						log.Error("failed to remove backend", "address", backend.Addr, "port", backend.Port, "err", err)
					}
					lb.backendMap[backend] = newStatus
				}
			}
		}
		lb.lock.Unlock()
	}, lb.interval, lb.stop)
}
