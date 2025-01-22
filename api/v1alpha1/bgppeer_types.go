/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"fmt"
	"net"

	"github.com/kube-vip/kube-vip/pkg/vip"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

func init() {
	SchemeBuilder.Register(&BGPPeer{}, &BGPPeerList{})
}

// BGPPeerSpec defines the desired state of BGPPeer.
type BGPPeerSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Address  string `json:"address"`
	AS       uint32 `json:"as,omitempty"`
	Password string `json:"password,omitempty"`
	MultiHop bool   `json:"multihop,omitempty"`

	// +kubebuilder:validation:Enum=fixed;auto_sourceif;auto_sourceip
	MpbgpNexthop string `json:"mpbgp_nexthop,omitempty"`
	MpbgpIPv4    string `json:"mpbgp_ipv4,omitempty"`
	MpbgpIPv6    string `json:"mpbgp_ipv6,omitempty"`
}

// BGPPeerStatus defines the observed state of BGPPeer.
type BGPPeerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=bgppeers,scope=Cluster

// BGPPeer is the Schema for the bgppeers API.
type BGPPeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPPeerSpec   `json:"spec,omitempty"`
	Status BGPPeerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BGPPeerList contains a list of BGPPeer.
type BGPPeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPPeer `json:"items"`
}

// BGPConfig defines the BGP server configuration
type BGPConfig struct {
	AS           uint32
	RouterID     string
	SourceIP     string
	SourceIF     string
	MpbgpNexthop string
	MpbgpIPv4    string
	MpbgpIPv6    string

	HoldTime          uint64
	KeepaliveInterval uint64

	Peers []BGPPeer
}

func (p *BGPPeer) SetMpbgpOptions(server *BGPConfig) {
	if p.Spec.MpbgpNexthop == "" {
		p.Spec.MpbgpNexthop = server.MpbgpNexthop
	}

	if p.Spec.MpbgpIPv4 == "" {
		p.Spec.MpbgpIPv4 = server.MpbgpIPv4
	}

	if p.Spec.MpbgpIPv6 == "" {
		p.Spec.MpbgpIPv6 = server.MpbgpIPv6
	}
}

func (p *BGPPeer) FindMpbgpAddresses(ap *api.Peer, server *BGPConfig) (string, string, error) {
	var ipv4Address, ipv6Address string
	switch p.Spec.MpbgpNexthop {
	case "fixed":
		ap.Transport.LocalAddress = server.SourceIP
		if p.Spec.MpbgpIPv4 == "" && p.Spec.MpbgpIPv6 == "" {
			return "", "", fmt.Errorf("to use MP-BGP with fixed address at least one IPv4 or IPv6 address has to be provided [current - IPv4: %s, IPv6: %s]",
				p.Spec.MpbgpIPv4, p.Spec.MpbgpIPv6)
		}

		if p.Spec.MpbgpIPv4 != "" {
			if net.ParseIP(p.Spec.MpbgpIPv4) == nil {
				return "", "", fmt.Errorf("provided address '%s' is not a valid IPv4 address", p.Spec.MpbgpIPv4)
			}
		}
		if p.Spec.MpbgpIPv6 != "" {
			if net.ParseIP(p.Spec.MpbgpIPv6) == nil {
				return "", "", fmt.Errorf("provided address '%s' is not a valid IPv6 address", p.Spec.MpbgpIPv6)
			}
		}

		ipv4Address = p.Spec.MpbgpIPv4
		ipv6Address = p.Spec.MpbgpIPv6
	case "auto_sourceip":
		ap.Transport.LocalAddress = server.SourceIP

		// Resolve the local interface by SourceIP
		iface, err := vip.GetInterfaceByIP(server.SourceIP)
		if err != nil {
			return "", "", fmt.Errorf("failed to get interface by IP: %v", err)
		}

		if vip.IsIPv4(server.SourceIP) {
			// Get the non link-local IPv6 address on that interface
			ipv6Address, err = vip.GetNonLinkLocalIP(iface, netlink.FAMILY_V6)
			if err != nil {
				return "", "", fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
			}
		} else {
			// Get the non link-local IPv4 address on that interface
			ipv4Address, err = vip.GetNonLinkLocalIP(iface, netlink.FAMILY_V4)
			if err != nil {
				return "", "", fmt.Errorf("failed to get non link-local IPv4 address: %v", err)
			}
		}
	case "auto_sourceif":
		ap.Transport.BindInterface = server.SourceIF

		iface, err := netlink.LinkByName(server.SourceIF)
		if err != nil {
			return "", "", fmt.Errorf("failed to get interface by name: %v", err)
		}

		// Get the non link-local IPv4 address on that interface
		ipv4Address, err = vip.GetNonLinkLocalIP(&iface, netlink.FAMILY_V4)
		if err != nil {
			return "", "", fmt.Errorf("failed to get non link-local IPv4 address: %v", err)
		}

		// Get the non link-local IPv6 address on that interface
		ipv6Address, err = vip.GetNonLinkLocalIP(&iface, netlink.FAMILY_V6)
		if err != nil {
			return "", "", fmt.Errorf("failed to get non link-local IPv6 address: %v", err)
		}
	default:
		return "", "", fmt.Errorf("option %s for MP-BPG nexthop is not supported", server.MpbgpNexthop)
	}

	return ipv4Address, ipv6Address, nil
}

func (c *BGPConfig) UpdatePeers(p *BGPPeerList) {
	pm := map[string]*BGPPeer{}
	for i := range c.Peers {
		pm[c.Peers[i].Spec.Address] = &c.Peers[i]
	}

	for i := range p.Items {
		pm[p.Items[i].Spec.Address].Spec = p.Items[i].Spec
	}
}
