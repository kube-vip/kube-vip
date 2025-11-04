package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	log "log/slog"

	"github.com/cloudflare/ipvs"
	"github.com/cloudflare/ipvs/netmask"
	"github.com/kube-vip/kube-vip/pkg/backend"
	"github.com/kube-vip/kube-vip/pkg/sysctl"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/vishvananda/netlink"
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
	networkInterface    string
	leaderCancel        context.CancelFunc
	signal              chan os.Signal
	address             string
	family              ipvs.AddressFamily
}

func NewIPVSLB(address string, port uint16, forwardingMethod string, backendHealthCheckInterval int, networkInterface string, leaderCancel context.CancelFunc, signal chan os.Signal) (*IPVSLoadBalancer, error) {
	log.Info("Starting IPVS LoadBalancer", "address", address)

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
		if errors.Is(err, os.ErrPermission) {
			log.Error("no permission to get IPVS info - please ensure that kube-vip is running with proper capabilities/privileged mode")
		}
		panic("")
	}
	log.Info("IPVS Loadbalancer enabled", "version", fmt.Sprintf("%d.%d.%d", i.Version[0], i.Version[1], i.Version[2]))

	ip, family := ipAndFamily(address)

	if strings.ToLower(forwardingMethod) == "masquerade" {
		enableProcSys("/proc/sys/net/ipv4/vs/conntrack", "net.ipv4.vs.conntrack")
		if family == ipvs.INET6 {
			enableProcSys("/proc/sys/net/ipv6/conf/all/forwarding", "net.ipv6.conf.all.forwarding")
		} else {
			enableProcSys("/proc/sys/net/ipv4/ip_forward", "net.ipv4.ip_forward")
		}
	}

	netMask := netmask.MaskFrom(31, vip.DefaultMaskIPv4) // For ipv4
	if family == ipvs.INET6 {
		netMask = netmask.MaskFrom(128, vip.DefaultMaskIPv6) // For ipv6
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
		stop:                make(chan struct{}),
		networkInterface:    networkInterface,
		leaderCancel:        leaderCancel,
		signal:              signal,
		address:             address,
		family:              family,
	}

	go lb.healthCheck()

	// Return our created load-balancer
	return lb, nil
}

func enableProcSys(path, name string) {
	isSet, err := sysctl.EnableProcSys(path)
	if err != nil {
		log.Error(fmt.Sprintf("ensuring %s enabled", name), "err", err)
		panic("")
	}
	if isSet {
		log.Info(fmt.Sprintf("sysctl set %s to 1", name))
	}
}

func (lb *IPVSLoadBalancer) RemoveIPVSLB() error {
	log.Info("Stopping IPVS LoadBalancer", "address", lb.address)
	close(lb.stop)
	err := lb.client.RemoveService(lb.loadBalancerService)
	if err != nil {
		return fmt.Errorf("error removing existing IPVS service: %v", err)
	}
	return nil
}

func (lb *IPVSLoadBalancer) AddBackend(address string, port uint16) error {
	isLocal := false
	var err error

	// Discard backend if it is of different IP family than LB address.
	if _, family := ipAndFamily(address); family != lb.family {
		return nil
	}

	if lb.forwardingMethod == ipvs.Local {
		log.Info("checking if backend is local", "addr", address)
		isLocal, err = lb.isLocal(address)
		if err != nil {
			log.Error("checking if backend is local", "err", err)
		}
	}

	backend := backend.Entry{Addr: address, Port: port, IsLocal: isLocal}

	lb.lock.Lock()
	defer lb.lock.Unlock()
	if _, ok := lb.backendMap[backend]; !ok {
		isHealth := backend.Check()
		if isHealth {
			err := lb.addBackend(address, port)
			if err != nil {
				return err
			}
		}
		lb.backendMap[backend] = isHealth
	}
	return nil
}

func (lb *IPVSLoadBalancer) addBackend(address string, port uint16) error {
	backend := backend.Entry{Addr: address, Port: port}
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
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
		defer lb.lock.Unlock()
		for backend, oldStatus := range lb.backendMap {
			newStatus := backend.Check()
			if newStatus {
				// old status -> health
				if !oldStatus {
					err := lb.addBackend(backend.Addr, backend.Port)
					if err != nil {
						log.Error("add backend", "err", err)
					}
					lb.backendMap[backend] = newStatus
				}
			} else {
				// old status -> not health
				if oldStatus {
					log.Info("healthCheck failed - removing backend", "address", backend.Addr, "port", backend.Port)
					err := lb.removeBackend(backend.Addr, backend.Port)
					if err != nil {
						log.Error("failed to remove backend", "address", backend.Addr, "port", backend.Port, "err", err)
					}
					lb.backendMap[backend] = newStatus
				}
				if lb.forwardingMethod == ipvs.Local && !lb.localBackendExists() {
					if lb.signal != nil {
						close(lb.signal)
					}

					if lb.leaderCancel != nil {
						lb.leaderCancel()
					}
				}
			}
		}
	}, lb.interval, lb.stop)
}

func (lb *IPVSLoadBalancer) isLocal(address string) (bool, error) {
	link, err := netlink.LinkByName(lb.networkInterface)
	if err != nil {
		return false, fmt.Errorf("getting link '%s': %w", lb.networkInterface, err)
	}

	family := netlink.FAMILY_V6
	if utils.IsIPv4(address) {
		family = netlink.FAMILY_V4
	}

	target := net.ParseIP(address)
	if target == nil {
		return false, fmt.Errorf("address '%s' is not a valid IP address", address)
	}

	addrs, err := netlink.AddrList(link, family)
	if err != nil {
		return false, fmt.Errorf("listing addresses for link '%s': %w", lb.networkInterface, err)
	}

	for _, addr := range addrs {
		if addr.IP.Equal(target) {
			return true, nil
		}
	}

	return false, nil
}

func (lb *IPVSLoadBalancer) localBackendExists() bool {
	for backend, isHealthy := range lb.backendMap {
		if backend.IsLocal && isHealthy {
			return true
		}
	}
	return false
}
