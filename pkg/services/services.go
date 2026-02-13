package services

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	log "log/slog"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/egress"
	"github.com/kube-vip/kube-vip/pkg/endpoints"
	"github.com/kube-vip/kube-vip/pkg/endpoints/providers"
	"github.com/kube-vip/kube-vip/pkg/instance"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/servicecontext"
	"github.com/kube-vip/kube-vip/pkg/upnp"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

type ServiceInstanceAction string

const (
	ActionDelete ServiceInstanceAction = "delete"
	ActionAdd    ServiceInstanceAction = "add"
	ActionNone   ServiceInstanceAction = "none"

	// Default UPNP lease requested is 3600 seconds.
	defaultUPNPLeaseDuration = 1 * time.Hour
)

func (p *Processor) SyncServices(ctx *servicecontext.Context, svc *v1.Service) error {
	log.Debug("[STARTING] Service Sync", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)

	// Iterate through the synchronising services

	action := p.getServiceInstanceAction(svc)
	switch action {
	case ActionDelete:
		// remove the label from the node before deleting the service
		if err := p.nodeLabelManager.RemoveLabel(ctx.Ctx, svc); err != nil {
			return fmt.Errorf("error removing label from node: %w", err)
		}

		log.Debug("[service] delete", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if err := p.deleteService(ctx.Ctx, svc.UID); err != nil {
			return fmt.Errorf("error deleting service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	case ActionAdd:
		log.Debug("[service] add", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
		if err := p.addService(ctx.Ctx, svc); err != nil {
			return fmt.Errorf("error adding service %s/%s: %w", svc.Namespace, svc.Name, err)
		}

		// add the label to the node after adding the service
		if err := p.nodeLabelManager.AddLabel(ctx.Ctx, svc); err != nil {
			return fmt.Errorf("error adding label to node: %w", err)
		}
	case ActionNone:
		log.Debug("[service] no action", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
	}
	log.Debug("[FINISHED] Service Sync", "namespace", svc.Namespace, "name", svc.Name, "uid", svc.UID)
	return nil
}

func (p *Processor) getServiceInstanceAction(svc *v1.Service) ServiceInstanceAction {
	// protect against multiple calls
	// get the annotations or legacy values from manual configuration
	addresses, hostnames := instance.FetchServiceAddresses(svc)
	// get the status information of the LB Service
	statusAddresses, _ := instance.FetchLoadBalancerIngress(svc)
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for _, instance := range p.ServiceInstances {
		if instance != nil && instance.ServiceSnapshot.UID == svc.UID {
			for _, address := range addresses {
				// handle the case where the service instance needs to be deleted
				if instance.IsDHCPv4 {
					if address != "0.0.0.0" {
						return ActionDelete
					}
					if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, instance.DHCPInterfaceIPv4) {
						return ActionDelete
					}
				} else {
					if address == "0.0.0.0" {
						return ActionDelete
					}
					if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, address) {
						return ActionDelete
					}
				}
				if instance.IsDHCPv6 {
					if address != "::" {
						return ActionDelete
					}
					if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, instance.DHCPInterfaceIPv6) {
						return ActionDelete
					}
				} else {
					if address == "::" {
						return ActionDelete
					}
					if len(svc.Status.LoadBalancer.Ingress) > 0 && !slices.Contains(statusAddresses, address) {
						return ActionDelete
					}
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && !comparePortsAndPortStatuses(svc) {
					return ActionDelete
				}
			}
			// If we reach here, it means the service instance matches the service UID and is not a DHCP service, so we can return "no action"
			return ActionNone
		}
	}
	if len(addresses) > 0 || len(hostnames) > 0 {
		log.Debug("no matching service instance found", "service", svc.Name, "namespace", svc.Namespace, "uid", svc.UID, "addresses", addresses, "hostnames", hostnames)
		return ActionAdd // If no matching instance is found, we need to add a new service instance
	}
	return ActionNone
}

func comparePortsAndPortStatuses(svc *v1.Service) bool {
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return false
	}
	portsStatus := svc.Status.LoadBalancer.Ingress[0].Ports
	if len(portsStatus) != len(svc.Spec.Ports) {
		return false
	}
	for i, portSpec := range svc.Spec.Ports {
		if portsStatus[i].Port != portSpec.Port || portsStatus[i].Protocol != portSpec.Protocol {
			return false
		}
	}
	return true
}

func (p *Processor) addService(ctx context.Context, svc *v1.Service) error {
	// protect against addService while reading
	p.mutex.Lock()
	defer p.mutex.Unlock()

	startTime := time.Now()

	newService, err := instance.NewInstance(ctx, svc, p.config, p.intfMgr, p.arpMgr)
	if err != nil {
		return err
	}

	for x := range newService.VIPConfigs {
		log.Debug("starting loadbalancer for service", "name", svc.Name, "namespace", svc.Namespace, "uid", svc.UID)
		if err := newService.Clusters[x].StartLoadBalancerService(ctx, newService.VIPConfigs[x], p.bgpServer, svc.Name, p.CountRouteReferences, &p.loadbalancersWg); err != nil {
			return fmt.Errorf("failed to start lb: %w", err)
		}
	}

	p.upnpMap(ctx, newService)

	if newService.IsDHCPv4 {
		go func() {
			index := -1
			for i := range newService.VIPConfigs {
				ip := net.ParseIP(newService.VIPConfigs[i].VIP)
				if ip.To4() != nil {
					index = i
					break
				}
			}
			if index == -1 {
				log.Error("unable to find proper VIPConfig for the DHCPv4")
			} else {
				for ip := range newService.DHCPv4Client.IPChannel() {
					log.Debug("IP changed", "ip", ip)
					newService.VIPConfigs[index].VIP = ip
					newService.DHCPInterfaceIPv4 = ip
					if !p.config.DisableServiceUpdates {
						if err := p.updateStatus(ctx, newService); err != nil {
							log.Warn("updating svc", "err", err)
						}
					}
				}
				log.Debug("IPv4 update channel closed, stopping")
			}

		}()
	}

	if newService.IsDHCPv6 {
		go func() {
			index := -1
			for i := range newService.VIPConfigs {
				ip := net.ParseIP(newService.VIPConfigs[i].VIP)
				if ip.To4() == nil {
					index = i
					break
				}
			}
			if index == -1 {
				log.Error("unable to find proper VIPConfig for the DHCPv6")
			} else {
				for ip := range newService.DHCPv4Client.IPChannel() {
					log.Debug("IP changed", "ip", ip)
					newService.VIPConfigs[index].VIP = ip
					newService.DHCPInterfaceIPv6 = ip
					if !p.config.DisableServiceUpdates {
						if err := p.updateStatus(ctx, newService); err != nil {
							log.Warn("updating svc", "err", err)
						}
					}
				}
				log.Debug("IPv6 update channel closed, stopping")
			}
		}()
	}

	p.ServiceInstances = append(p.ServiceInstances, newService)

	if !p.config.DisableServiceUpdates {
		log.Debug("[service] update", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name)
		if err := p.updateStatus(ctx, newService); err != nil {
			log.Error("[service] updating status", "namespace", newService.ServiceSnapshot.Namespace, "name", newService.ServiceSnapshot.Name, "err", err)
		}
	}

	serviceIPs, _ := instance.FetchServiceAddresses(svc)
	// Check if we need to flush any conntrack connections (due to some dangling conntrack connections)
	if svc.Annotations[kubevip.FlushContrack] == "true" {

		log.Debug("[service] Flushing conntrack rules", "service", svc.Name, "namespace", svc.Namespace)
		for _, serviceIP := range serviceIPs {
			err = vip.DeleteExistingSessions(serviceIP, false, svc.Annotations[kubevip.EgressDestinationPorts], svc.Annotations[kubevip.EgressSourcePorts])
			if err != nil {
				log.Error("[service] flushing any remaining egress connections", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
			err = vip.DeleteExistingSessions(serviceIP, true, svc.Annotations[kubevip.EgressDestinationPorts], svc.Annotations[kubevip.EgressSourcePorts])
			if err != nil {
				log.Error("[service] flushing any remaining ingress connections", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
	}

	// Check if egress is enabled on the service, if so we'll need to configure some rules
	if svc.Annotations[kubevip.Egress] == "true" && len(serviceIPs) > 0 {
		log.Debug("[service] enabling egress", "service", svc.Name, "namespace", svc.Namespace)
		// If we'er not using NFtables, then ensure that the correct iptables modules are loaded
		if p.config.EgressWithNftables {
			// Ensure that kernel modules are loaded and report back missing modules.
			err = p.nftablesCheck()
			if err != nil {
				log.Warn("[service] configuring nft egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		} else {
			// Ensure that kernel modules are loaded and report back missing modules.
			err = p.iptablesCheck()
			if err != nil {
				log.Warn("[service] configuring egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
		var podIP string
		errList := []error{}

		// Should egress be IPv6
		if svc.Annotations[kubevip.EgressIPv6] == "true" {
			// Does the service have an active IPv6 endpoint
			if svc.Annotations[kubevip.ActiveEndpointIPv6] != "" {
				for _, serviceIP := range serviceIPs {
					if !p.config.EnableEndpoints && utils.IsIPv6(serviceIP) {

						podIP = svc.Annotations[kubevip.ActiveEndpointIPv6]

						err = p.configureEgress(ctx, serviceIP, podIP, svc.Namespace, string(svc.UID), svc.Annotations)
						if err != nil {
							errList = append(errList, err)
							log.Warn("[service] configuring egress IPv6", "service", svc.Name, "namespace", svc.Namespace, "err", err)
						}
					}
				}
			}
		} else if svc.Annotations[kubevip.ActiveEndpoint] != "" { // Not expected to be IPv6, so should be an IPv4 address
			for _, serviceIP := range serviceIPs {
				podIPs := svc.Annotations[kubevip.ActiveEndpoint]
				if !p.config.EnableEndpoints && utils.IsIPv6(serviceIP) {
					podIPs = svc.Annotations[kubevip.ActiveEndpointIPv6]
				}
				err = p.configureEgress(ctx, serviceIP, podIPs, svc.Namespace, string(svc.UID), svc.Annotations)
				if err != nil {
					errList = append(errList, err)
					log.Warn("[service] configuring egress IPv4", "service", svc.Name, "namespace", svc.Namespace, "err", err)
				}
			}
		}
		if len(errList) == 0 {
			var provider providers.Provider
			if p.config.EnableEndpoints {
				provider = providers.NewEndpoints()
			} else {
				provider = providers.NewEndpointslices()
			}
			err = provider.UpdateServiceAnnotation(ctx, svc.Annotations[kubevip.ActiveEndpoint], svc.Annotations[kubevip.ActiveEndpointIPv6], svc, p.clientSet)
			if err != nil {
				log.Warn("[service] configuring egress", "service", svc.Name, "namespace", svc.Namespace, "err", err)
			}
		}
	}

	finishTime := time.Since(startTime)
	log.Info("[service]", "service", svc.Name, "namespace", svc.Namespace, "synchronised in", fmt.Sprintf("%dms", finishTime.Milliseconds()))

	return nil
}

func (p *Processor) deleteService(ctx context.Context, uid types.UID) error {
	// protect multiple calls
	p.mutex.Lock()
	defer p.mutex.Unlock()

	var updatedInstances []*instance.Instance
	var serviceInstance *instance.Instance
	found := false
	for x := range p.ServiceInstances {
		log.Debug("[service] lookup", "target UID", uid, "found UID", p.ServiceInstances[x].ServiceSnapshot.UID, "name", p.ServiceInstances[x].ServiceSnapshot.Name, "namespace", p.ServiceInstances[x].ServiceSnapshot.Namespace)
		// Add the running services to the new array
		if p.ServiceInstances[x].ServiceSnapshot.UID != uid {
			updatedInstances = append(updatedInstances, p.ServiceInstances[x])
		} else {
			// Flip the found when we match
			found = true
			serviceInstance = p.ServiceInstances[x]
		}
	}

	// If we've been through all services and not found the correct one then error
	if !found {
		// TODO: - fix UX
		// return fmt.Errorf("unable to find/stop service [%s]", uid)
		log.Warn("unable to find/stop service", "uid", uid)
		return nil
	}

	for _, c := range serviceInstance.Clusters {
		for n := range c.Network {
			c.Network[n].SetHasEndpoints(false)
		}
	}

	// Determine if this VIP is shared with other loadbalancers
	shared := false
	vipSet := make(map[string]interface{})
	for x := range updatedInstances {
		vips, _ := instance.FetchServiceAddresses(updatedInstances[x].ServiceSnapshot)
		for _, vip := range vips { //updatedInstances[x].ServiceSnapshot.Spec.LoadBalancerIP {
			vipSet[vip] = nil
		}
	}
	vips, _ := instance.FetchServiceAddresses(serviceInstance.ServiceSnapshot)
	for _, vip := range vips {
		if _, found := vipSet[vip]; found {
			shared = true
		}
	}
	if !shared {
		for x := range serviceInstance.Clusters {
			serviceInstance.Clusters[x].Stop()
		}
		if serviceInstance.IsDHCPv4 || serviceInstance.IsDHCPv6 {
			if serviceInstance.IsDHCPv4 {
				serviceInstance.DHCPv4Client.Stop()
			}

			if serviceInstance.IsDHCPv6 {
				serviceInstance.DHCPv6Client.Stop()
			}

			macvlan, err := netlink.LinkByName(serviceInstance.DHCPInterface)
			if err != nil {
				return fmt.Errorf("[service] error finding VIP Interface: %v", err)
			}

			err = netlink.LinkDel(macvlan)
			if err != nil {
				return fmt.Errorf("[service] error deleting DHCP Link : %v", err)
			}
		}

		if p.config.EnableBGP {
			endpoints.ClearBGPHostsByInstance(ctx, serviceInstance, p.bgpServer)
		}
		if p.config.EnableRoutingTable && (p.config.EnableLeaderElection || p.config.EnableServicesElection) {
			if errs := endpoints.ClearRoutesByInstance(serviceInstance.ServiceSnapshot, serviceInstance, &p.ServiceInstances); len(errs) > 0 {
				for _, err := range errs {
					log.Error("unable to clear routes", "err", err)
				}
			}
		}

		// We will need to tear down the egress
		if serviceInstance.ServiceSnapshot.Annotations[kubevip.Egress] == "true" {
			if serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint] != "" {
				log.Info("[service] egress re-write enabled", "service", serviceInstance.ServiceSnapshot.Name)
				err := egress.Teardown(serviceInstance.ServiceSnapshot.Annotations[kubevip.ActiveEndpoint], serviceInstance.ServiceSnapshot.Spec.LoadBalancerIP, serviceInstance.ServiceSnapshot.Namespace, string(serviceInstance.ServiceSnapshot.UID), serviceInstance.ServiceSnapshot.Annotations, p.config.EgressWithNftables)
				if err != nil {
					log.Error("[service] egress teardown", "err", err)
				}
			}
		}
	}

	// Update the service array
	p.ServiceInstances = updatedInstances

	log.Info("Removed instance from manager", "uid", uid, "name", serviceInstance.ServiceSnapshot.Name, "remaining advertised services", len(p.ServiceInstances))

	return nil
}

// upnpLeaseDurationForService determines the UPNP lease duration for a given service, based on its annotations.
//
// The default lease duration is set to 1 hour, maintaining the default of 3600 seconds that was previously passed. If
// the service has an annotation of [kubevip.UpnpLeaseDuration], the function attempts to parse its value as a
// [time.Duration] using [time.ParseDuration].
//
// If parsing is successful, the lease duration is updated accordingly; otherwise, a warning is logged and the default
// duration is retained.
//
// Overriding the default lease duration can be useful for services that require longer or shorter UPNP port mappings,
// or for buggy UPNP implementations that may not handle renewals correctly. At least one router's implementation
// completely times out the mapping very shortly after creation if it is set to 3600 or 7200, but works fine if 0 is
// used.
//
// This function must therefore explicitly permit duration of 0, and callers and the underlying library must pass that
// value in XML correctly. A duration of 0 indicates to the UPNP gateway that the mapping should be permanent.
//
// It may be useful to update this function to read a global configuration option as well. This helper could also take
// in v1.Service instead of [instance.Instance], but the latter is more convenient for callers.
//
// Example where 0 was observed to stay on the problematic router: miniupnpc's test client, upnpc v2.2.4.
func upnpLeaseDurationForService(s *instance.Instance) time.Duration {
	if s == nil || s.ServiceSnapshot == nil || s.ServiceSnapshot.Annotations == nil {
		// No warning output. No annotation is unusual but perfectly ok.
		return defaultUPNPLeaseDuration
	}

	// Constant is named `UpnpLeaseDuration` for consistency with `UpnpEnabled`. According to Go naming conventions
	// regarding use of acronyms, `UPNPLeaseDuration` would be preferred. Cleanup of both of these is left for a future
	// refactor as these are public symbols and might be used elsewhere in the ecosystem.
	val, ok := s.ServiceSnapshot.Annotations[kubevip.UpnpLeaseDuration]
	if !ok {
		// No warning output. No annotation is common and perfectly ok.
		return defaultUPNPLeaseDuration
	}

	if val == "" {
		log.Warn("[UPNP] Lease duration annotation is empty, using default of 1 hour", "service", s.ServiceSnapshot.Name)
		return defaultUPNPLeaseDuration
	}

	parsed, err := time.ParseDuration(val)
	if err != nil {
		log.Warn("[UPNP] Unable to parse lease duration from annotation, using default of 1 hour", "service", s.ServiceSnapshot.Name, "err", err)
		return defaultUPNPLeaseDuration
	}

	if parsed < 0 {
		log.Warn("[UPNP] Lease duration from annotation is negative, using default of 1 hour", "service", s.ServiceSnapshot.Name)
		return defaultUPNPLeaseDuration
	}

	return parsed
}

// upnpLeaseDurationForServiceSec returns the UPNP lease duration for a service in uint32 seconds, as expected by the
// helper library. This is a convenience wrapper around [upnpLeaseDurationForService], and in case
// upnpLeaseDurationForService returns a duration that maps to a negative value of seconds or invalid float of seconds,
// it will return the default lease duration in seconds instead. (Technically, it will check for a reasonable range of
// seconds, e.g. ~10 years-ish.)
func upnpLeaseDurationForServiceSec(s *instance.Instance) uint32 {
	duration := upnpLeaseDurationForService(s)
	seconds := duration.Seconds()
	// Check if within range.
	if seconds >= 0 && seconds <= float64(10*365*24*60*60) {
		return uint32(seconds)
	}
	return uint32(defaultUPNPLeaseDuration.Seconds())
}

// Set up UPNP forwards for a service
// We first try to use the more modern Pinhole API introduced in UPNPv2 and fall back to UPNPv2 Port Forwarding if no forward was successful
func (p *Processor) upnpMap(ctx context.Context, s *instance.Instance) {
	if !isUPNPEnabled(s.ServiceSnapshot) {
		// Skip services missing the annotation
		return
	}
	if !p.config.EnableUPNP {
		log.Warn("[UPNP] Found kube-vip.io/forwardUPNP on service while UPNP forwarding is disabled in the kube-vip config. Not forwarding", "service", s.ServiceSnapshot.Name)
		return
	}
	// If upnp is enabled then update the gateway/router with the address
	// TODO - check if this implementation for dualstack is correct

	gateways := upnp.GetGatewayClients(ctx)

	// Determine desired UPNP TTL / "lease duration". Passed into the library as integer seconds from now, as the
	// underlying XML API wants integer seconds.
	leaseDurationSec := upnpLeaseDurationForServiceSec(s)

	// Reset Gateway IPs to remove stale addresses
	s.UPNPGatewayIPs = make([]string, 0)

	vips, _ := instance.FetchServiceAddresses(s.ServiceSnapshot)
	for _, vip := range vips {
		for _, port := range s.ServiceSnapshot.Spec.Ports {
			for _, gw := range gateways {

				forwardSucessful := false
				if gw.WANIPv6FirewallControlClient != nil {
					log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "gateway", gw.WANIPv6FirewallControlClient.Location, "leaseDurationSec", leaseDurationSec)

					pinholeID, pinholeErr := gw.WANIPv6FirewallControlClient.AddPinholeCtx(ctx, "0.0.0.0", uint16(port.Port), vip, uint16(port.Port), upnp.MapProtocolToIANA(string(port.Protocol)), leaseDurationSec) //nolint  TODO
					if pinholeErr == nil {
						forwardSucessful = true
						log.Info("[UPNP] Service should be accessible externally", "port", port.Port, "pinhold ID", pinholeID)
					} else {
						//TODO: Cleanup
						log.Error("[UPNP] Unable to map port to gateway using Pinhole API", "err", pinholeErr.Error())
					}
				}
				// Fallback to PortForward
				if !forwardSucessful {
					log.Info("[UPNP] Adding map", "vip", vip, "port", port.Port, "service", s.ServiceSnapshot.Name, "leaseDurationSec", leaseDurationSec)

					portMappingErr := gw.ConnectionClient.AddPortMapping("0.0.0.0", uint16(port.Port), strings.ToUpper(string(port.Protocol)), uint16(port.Port), vip, true, s.ServiceSnapshot.Name, leaseDurationSec) //nolint  TODO
					if portMappingErr == nil {
						ip, err := gw.ConnectionClient.GetExternalIPAddress()
						if err != nil {
							// Log the error but continue on the off chance the mapping was successful
							log.Error("[UPNP] Unable to get external IP address from gateway", "service", s.ServiceSnapshot.Name, "port", port.Port, "err", err)
						} else {
							log.Info("[UPNP] Service should be accessible externally", "service", s.ServiceSnapshot.Name, "port", port.Port, "externalip", ip)
						}
						forwardSucessful = true
					} else {
						//TODO: Cleanup
						log.Error("[UPNP] Unable to map port to gateway using PortForward API", "err", portMappingErr.Error())
					}
				}

				if forwardSucessful {
					ip, err := gw.ConnectionClient.GetExternalIPAddress()
					if err == nil {
						s.UPNPGatewayIPs = append(s.UPNPGatewayIPs, ip)
					}
				}
			}
		}
	}

	// Remove duplicate IPs
	slices.Sort(s.UPNPGatewayIPs)
	s.UPNPGatewayIPs = slices.Compact(s.UPNPGatewayIPs)
}

func (p *Processor) updateStatus(ctx context.Context, i *instance.Instance) error {
	// let's retry status update every 10ms for 30s
	retryConfig := wait.Backoff{
		Steps:    3000,
		Duration: 10 * time.Millisecond,
		Factor:   0,
		Jitter:   0.1,
	}
	// will retry for every error encountered, TODO: should a list of errors that will trigger retry be specified?
	err := retry.OnError(retryConfig, func(error) bool { return true }, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, err := p.clientSet.CoreV1().Services(i.ServiceSnapshot.Namespace).Get(ctx, i.ServiceSnapshot.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		currentServiceCopy := currentService.DeepCopy()
		if currentServiceCopy.Annotations == nil {
			currentServiceCopy.Annotations = make(map[string]string)
		}

		// If we're using ARP then we can only broadcast the VIP from one place, also useful for other software when running BGP, add an annotation to the service
		if p.config.EnableARP || p.config.EnableBGP {
			// Add the current host
			currentServiceCopy.Annotations[kubevip.VipHost] = p.config.NodeName
		}
		if i.DHCPInterfaceHwaddr != "" || i.DHCPInterfaceIPv4 != "" || i.DHCPInterfaceIPv6 != "" {
			currentServiceCopy.Annotations[kubevip.HwAddrKey] = i.DHCPInterfaceHwaddr
			dhcpInterfaceIP := ""
			if i.DHCPInterfaceIPv4 != "" {
				dhcpInterfaceIP = i.DHCPInterfaceIPv4
				if i.DHCPInterfaceIPv6 != "" {
					dhcpInterfaceIP += ","
				}
			}
			if i.DHCPInterfaceIPv6 != "" {
				dhcpInterfaceIP += i.DHCPInterfaceIPv6
			}
			currentServiceCopy.Annotations[kubevip.RequestedIP] = dhcpInterfaceIP
		}

		if currentService.Annotations["development.kube-vip.io/synthetic-api-server-error-on-update"] == "true" {
			log.Error("(Synthetic error ) updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
			return fmt.Errorf("(Synthetic) simulating api server errors")
		}

		if !cmp.Equal(currentService, currentServiceCopy) {
			currentService, err = p.clientSet.CoreV1().Services(currentServiceCopy.Namespace).Update(ctx, currentServiceCopy, metav1.UpdateOptions{})
			if err != nil {
				log.Error("updating Spec", "service", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}

		ports := make([]v1.PortStatus, 0, len(i.ServiceSnapshot.Spec.Ports))
		for _, port := range i.ServiceSnapshot.Spec.Ports {
			ports = append(ports, v1.PortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
			})
		}

		ingresses := []v1.LoadBalancerIngress{}

		for _, c := range i.VIPConfigs {
			if !utils.IsIP(c.VIP) {
				ips, err := utils.LookupHost(c.VIP, c.DNSMode, *i.ServiceSnapshot.Spec.IPFamilyPolicy == v1.IPFamilyPolicyRequireDualStack)
				if err != nil {
					return err
				}
				for _, ip := range ips {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingresses = append(ingresses, i)
				}
			} else {
				i := v1.LoadBalancerIngress{
					IP:    c.VIP,
					Ports: ports,
				}
				ingresses = append(ingresses, i)
			}
			if isUPNPEnabled(currentService) {
				for _, ip := range i.UPNPGatewayIPs {
					i := v1.LoadBalancerIngress{
						IP:    ip,
						Ports: ports,
					}
					ingresses = append(ingresses, i)
				}
			}
		}
		log.Debug("LB status", "current", currentService.Status.LoadBalancer.Ingress, "new", ingresses)
		if !ingressEqual(currentService.Status.LoadBalancer.Ingress, ingresses) {
			currentService.Status.LoadBalancer.Ingress = ingresses
			log.Debug("updating service status", "namespace", currentService.Namespace, "name", currentService.Name, "uid", currentService.UID)
			_, err = p.clientSet.CoreV1().Services(currentService.Namespace).UpdateStatus(ctx, currentService, metav1.UpdateOptions{})
			if err != nil && !apierrors.IsInvalid(err) {
				log.Error("updating Service", "namespace", i.ServiceSnapshot.Namespace, "name", i.ServiceSnapshot.Name, "err", err)
				return err
			}
		}
		return nil
	})

	return err
}

func ingressEqual(a, b []v1.LoadBalancerIngress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if a[i].IP != b[i].IP || a[i].Hostname != b[i].Hostname ||
			!cmp.Equal(a[i].Ports, b[i].Ports) {
			return false
		}
	}
	return true
}

func isUPNPEnabled(s *v1.Service) bool {
	return metav1.HasAnnotation(s.ObjectMeta, kubevip.UpnpEnabled) && s.Annotations[kubevip.UpnpEnabled] == "true"
}

// Refresh UPNP Port Forwards for all Service Instances registered in the processor
func (p *Processor) RefreshUPNPForwards(ctx context.Context) {
	log.Info("Starting UPNP Port Refresher")
	for {
		time.Sleep(300 * time.Second)

		log.Info("[UPNP] Refreshing Instances", "number of instances", len(p.ServiceInstances))
		for i := range p.ServiceInstances {
			p.upnpMap(ctx, p.ServiceInstances[i])
			if err := p.updateStatus(ctx, p.ServiceInstances[i]); err != nil {
				log.Warn("[UPNP] Error updating service", "ip", p.ServiceInstances[i].ServiceSnapshot.Name, "err", err)
			}
		}
	}
}
