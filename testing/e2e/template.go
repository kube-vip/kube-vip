//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net"
)

type KubevipManifestValues struct {
	ControlPlaneVIP                         string
	ImagePath                               string
	ConfigPath                              string
	SvcEnable                               string
	SvcElectionEnable                       string
	VipElectionEnable                       string
	EnableEndpoints                         string
	EnableNodeLabeling                      string
	ControlPlaneEnable                      string
	BGPAS                                   uint32
	BGPPeers                                string
	MPBGPNexthop                            string
	MPBGPNexthopIPv4                        string
	MPBGPNexthopIPv6                        string
	Mode                                    string
	Annotations                             string
	ControlPlaneHealthCheckAddress          string
	ControlPlaneHealthCheckPeriodSeconds    int
	ControlPlaneHealthCheckTimeoutSeconds   int
	ControlPlaneHealthCheckFailureThreshold int
	ControlPlaneHealthCheckCAPath           string
	EnableServiceSecurity                   string
	PerServiceElectionOnDemand              string
	PrometheusHTTPServer                    string
}

type BGPPeerValues struct {
	IP       string
	AS       uint32
	Port     uint16
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

	port := pv.Port
	if port == 0 {
		port = 179
	}

	if pv.MPBGP != "" {
		return fmt.Sprintf("%s:%d::false:%d:mpbgp_nexthop=%s", tmpIP, pv.AS, port, pv.MPBGP)
	}

	return fmt.Sprintf("%s:%d::false:%d", tmpIP, pv.AS, port)
}
