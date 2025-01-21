package bgp

import (
	"github.com/kube-vip/kube-vip/api/v1alpha1"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
)

// Server manages a server object
type Server struct {
	s *gobgp.BgpServer
	c *v1alpha1.BGPConfig
}
