//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net"
)

type KubevipManifestValues struct {
	ControlPlaneVIP      string
	ImagePath            string
	ConfigPath           string
	SvcEnable            string
	SvcElectionEnable    string
	EnableEndpointslices string
	EnableNodeLabeling   string
	ControlPlaneEnable   string
	BGPAS                uint32
	BGPPeers             string
	MPBGPNexthop         string
	MPBGPNexthopIPv4     string
	MPBGPNexthopIPv6     string
}

type BGPPeerValues struct {
	IP       string
	AS       uint32
	MPBGP    string
	IPFamily string
}

func (pv *BGPPeerValues) String() string {
	tmpIP := pv.IP
	ip := net.ParseIP(tmpIP)
	if ip == nil {
		return ""
	}

	if ip.To4() == nil {
		tmpIP = fmt.Sprintf("[%s]", tmpIP)
	}

	if pv.MPBGP != "" {
		return fmt.Sprintf("%s:%d::false/mpbgp_nexthop=%s", tmpIP, pv.AS, pv.MPBGP)
	}

	return fmt.Sprintf("%s:%d::false", tmpIP, pv.AS)
}
