package bgp

import gobgp "github.com/osrg/gobgp/v3/pkg/server"

// Peer defines a BGP Peer
type Peer struct {
	Address  string
	AS       uint32
	Password string
	MultiHop bool
}

// Config defines the BGP server configuration
type Config struct {
	AS       uint32
	RouterID string
	SourceIP string
	SourceIF string

	HoldTime          uint64
	KeepaliveInterval uint64

	Peers []Peer
}

// Server manages a server object
type Server struct {
	s *gobgp.BgpServer
	c *Config
}
