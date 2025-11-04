package manager

import (
	"fmt"
	"os"
	"time"
)

// dumpConfiguration prints the current configuration to stdout when SIGUSR1 is received
func (sm *Manager) dumpConfiguration() {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	fmt.Printf("\n")
	fmt.Printf("================================================================================\n")
	fmt.Printf("                   KUBE-VIP CONFIGURATION DUMP\n")
	fmt.Printf("================================================================================\n")
	fmt.Printf("Timestamp: %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("Node Name: %s\n", sm.config.NodeName)
	fmt.Printf("Process ID: %d\n", os.Getpid())
	fmt.Printf("================================================================================\n")
	fmt.Printf("\n")

	sm.dumpConfigSection()
	sm.dumpBGPSection()
	sm.dumpARPSection()
	sm.dumpServicesSection()
	sm.dumpNetworkInterfacesSection()
	sm.dumpLeaderElectionSection()
	sm.dumpRuntimeSection()

	fmt.Printf("================================================================================\n")
	fmt.Printf("                   END OF CONFIGURATION DUMP\n")
	fmt.Printf("================================================================================\n")
	fmt.Printf("\n")
}

func (sm *Manager) dumpConfigSection() {
	fmt.Printf("--- BASIC CONFIGURATION ---\n")
	fmt.Printf("VIP: %s\n", sm.config.Address)
	fmt.Printf("VIP Subnet: %s\n", sm.config.VIPSubnet)
	fmt.Printf("Port: %d\n", sm.config.Port)
	fmt.Printf("Namespace: %s\n", sm.config.Namespace)
	fmt.Printf("Service Namespace: %s\n", sm.config.ServiceNamespace)
	fmt.Printf("Interface: %s\n", sm.config.Interface)
	fmt.Printf("Services Interface: %s\n", sm.config.ServicesInterface)
	fmt.Printf("Single Node Mode: %t\n", sm.config.SingleNode)
	fmt.Printf("Start As Leader: %t\n", sm.config.StartAsLeader)
	fmt.Printf("\n")
}

func (sm *Manager) dumpBGPSection() {
	fmt.Printf("--- BGP CONFIGURATION ---\n")
	fmt.Printf("BGP Enabled: %t\n", sm.config.EnableBGP)
	if sm.config.EnableBGP {
		fmt.Printf("BGP AS: %d\n", sm.config.BGPConfig.AS)
		fmt.Printf("BGP Router ID: %s\n", sm.config.BGPConfig.RouterID)
		fmt.Printf("BGP Source IP: %s\n", sm.config.BGPConfig.SourceIP)
		fmt.Printf("BGP Source Interface: %s\n", sm.config.BGPConfig.SourceIF)
		fmt.Printf("BGP Hold Time: %d\n", sm.config.BGPConfig.HoldTime)
		fmt.Printf("BGP Keepalive Interval: %d\n", sm.config.BGPConfig.KeepaliveInterval)
		fmt.Printf("BGP Peers: %d\n", len(sm.config.BGPConfig.Peers))
		for i, peer := range sm.config.BGPConfig.Peers {
			fmt.Printf("  Peer %d: %s:%d (AS: %d, MultiHop: %t)\n",
				i+1, peer.Address, peer.Port, peer.AS, peer.MultiHop)
		}
	}
	fmt.Printf("\n")
}

func (sm *Manager) dumpARPSection() {
	fmt.Printf("--- ARP/NDP CONFIGURATION ---\n")
	fmt.Printf("ARP Enabled: %t\n", sm.config.EnableARP)
	if sm.config.EnableARP {
		fmt.Printf("ARP Broadcast Rate: %d\n", sm.config.ArpBroadcastRate)
	}
	fmt.Printf("Wireguard Enabled: %t\n", sm.config.EnableWireguard)
	fmt.Printf("Routing Table Enabled: %t\n", sm.config.EnableRoutingTable)
	if sm.config.EnableRoutingTable {
		fmt.Printf("Routing Table ID: %d\n", sm.config.RoutingTableID)
		fmt.Printf("Routing Protocol: %d\n", sm.config.RoutingProtocol)
		fmt.Printf("Clean Routing Table: %t\n", sm.config.CleanRoutingTable)
	}
	fmt.Printf("\n")
}

func (sm *Manager) dumpServicesSection() {
	fmt.Printf("--- SERVICES CONFIGURATION ---\n")
	fmt.Printf("Services Enabled: %t\n", sm.config.EnableServices)
	if sm.config.EnableServices {
		fmt.Printf("Services Election: %t\n", sm.config.EnableServicesElection)
		fmt.Printf("Load Balancer Class Only: %t\n", sm.config.LoadBalancerClassOnly)
		fmt.Printf("Load Balancer Class Name: %s\n", sm.config.LoadBalancerClassName)
		fmt.Printf("Disable Service Updates: %t\n", sm.config.DisableServiceUpdates)
		fmt.Printf("Enable Endpoints: %t\n", sm.config.EnableEndpoints)
		fmt.Printf("Service Security Enabled: %t\n", sm.config.EnableServiceSecurity)

		if sm.svcProcessor != nil {
			instances := sm.svcProcessor.ServiceInstances
			fmt.Printf("Active Service Instances: %d\n", len(instances))
			for i, inst := range instances {
				if inst.ServiceSnapshot != nil {
					svc := inst.ServiceSnapshot
					vipConfigs := ""
					for j, cfg := range inst.VIPConfigs {
						if j > 0 {
							vipConfigs += ", "
						}
						vipConfigs += cfg.Address
					}
					fmt.Printf("  Service %d: %s/%s (Type: %s, VIPs: %s)\n",
						i+1, svc.Namespace, svc.Name, svc.Spec.Type, vipConfigs)
				}
			}
		}
	}
	fmt.Printf("\n")
}

func (sm *Manager) dumpNetworkInterfacesSection() {
	fmt.Printf("--- NETWORK INTERFACES ---\n")
	fmt.Printf("Network Interface Manager: %t\n", sm.intfMgr != nil)
	fmt.Printf("ARP Manager: %t\n", sm.arpMgr != nil)
	fmt.Printf("\n")
}

func (sm *Manager) dumpLeaderElectionSection() {
	fmt.Printf("--- LEADER ELECTION CONFIGURATION ---\n")
	fmt.Printf("Control Plane Enabled: %t\n", sm.config.EnableControlPlane)
	if sm.config.EnableControlPlane {
		fmt.Printf("Detect Control Plane: %t\n", sm.config.DetectControlPlane)
	}
	fmt.Printf("Leader Election Type: %s\n", sm.config.LeaderElectionType)
	fmt.Printf("Leader Election Enabled: %t\n", sm.config.EnableLeaderElection)
	if sm.config.EnableLeaderElection {
		fmt.Printf("Lease Name: %s\n", sm.config.LeaseName)
		fmt.Printf("Lease Duration: %d seconds\n", sm.config.LeaseDuration)
		fmt.Printf("Renew Deadline: %d seconds\n", sm.config.RenewDeadline)
		fmt.Printf("Retry Period: %d seconds\n", sm.config.RetryPeriod)
	}
	fmt.Printf("Services Lease Name: %s\n", sm.config.ServicesLeaseName)
	fmt.Printf("Node Labeling Enabled: %t\n", sm.config.EnableNodeLabeling)
	fmt.Printf("\n")
}

func (sm *Manager) dumpRuntimeSection() {
	fmt.Printf("--- RUNTIME STATISTICS ---\n")
	fmt.Printf("Load Balancer Enabled: %t\n", sm.config.EnableLoadBalancer)
	if sm.config.EnableLoadBalancer {
		fmt.Printf("Load Balancer Port: %d\n", sm.config.LoadBalancerPort)
		fmt.Printf("Load Balancer Forwarding Method: %s\n", sm.config.LoadBalancerForwardingMethod)
		fmt.Printf("Load Balancers Configured: %d\n", len(sm.config.LoadBalancers))
	}
	fmt.Printf("Prometheus HTTP Server: %s\n", sm.config.PrometheusHTTPServer)
	fmt.Printf("Health Check Port: %d\n", sm.config.HealthCheckPort)
	fmt.Printf("UPNP Enabled: %t\n", sm.config.EnableUPNP)
	fmt.Printf("Egress Clean Enabled: %t\n", sm.config.EgressClean)
	if sm.config.EgressClean {
		fmt.Printf("Egress with nftables: %t\n", sm.config.EgressWithNftables)
		fmt.Printf("Egress Pod CIDR: %s\n", sm.config.EgressPodCidr)
		fmt.Printf("Egress Service CIDR: %s\n", sm.config.EgressServiceCidr)
	}
	fmt.Printf("\n")
}
