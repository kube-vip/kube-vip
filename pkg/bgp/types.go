package bgp

import gobgp "github.com/osrg/gobgp/pkg/server"

// Peer defines a BGP Peer
type Peer struct {
	Address  string
	AS       uint32
	Password string
}

// Config defines the BGP server configuration
type Config struct {
	AS       uint32
	RouterID string
	NextHop  string
	SourceIP string
	SourceIF string

	Peers []Peer
	IPv6  bool
}

// Server manages a server object
type Server struct {
	s *gobgp.BgpServer
	c *Config
}
