//go:build e2e
// +build e2e

// Package bgp provides reusable GoBGP test infrastructure for e2e tests.
//
// Two main types:
//   - Server: manages a GoBGP daemon and gRPC client. Shared across scenarios.
//   - Server.AddClusterPeers: integration helper to peer with Kind cluster nodes.
package bgp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"sigs.k8s.io/kind/pkg/cluster/nodes"

	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/services/pkg/deployment"
)

const (
	GoBGPAS   uint32 = 65500
	KubevipAS uint32 = 65501
	GoBGPPort uint32 = 50051
)

// ---------------------------------------------------------------------------
// Server — GoBGP daemon lifecycle + gRPC client
// ---------------------------------------------------------------------------

// Server manages a GoBGP daemon and provides a gRPC client for route
// verification and peer management. Create with NewServer, tear down with Stop.
type Server struct {
	Client    api.GoBgpServiceClient
	LocalIPv4 string
	LocalIPv6 string
	TempDir   string

	kill chan any
	done chan error
}

// NewServer starts a GoBGP daemon and connects a gRPC client.
// tempDir is used for the GoBGP config file; the caller owns its lifecycle.
func NewServer(tempDir string) *Server {
	s := &Server{TempDir: tempDir}

	// Detect local IPs on the Docker bridge
	networkInterface := os.Getenv("NETWORK_INTERFACE")
	if networkInterface == "" {
		networkInterface = "br-"
	}
	v4addr, _, err := deployment.GetLocalIPv4(networkInterface)
	Expect(err).ToNot(HaveOccurred())
	s.LocalIPv4 = v4addr.String()

	v6addr, _, err := deployment.GetLocalIPv6(networkInterface)
	Expect(err).ToNot(HaveOccurred())
	s.LocalIPv6 = v6addr.String()

	// Render GoBGP config and start daemon
	curDir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())

	tmpl, err := template.New("config.toml.tmpl").
		ParseFiles(filepath.Join(curDir, "bgp", "config.toml.tmpl"))
	Expect(err).ToNot(HaveOccurred())

	configPath := filepath.Join(tempDir, "config.toml")
	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	Expect(err).ToNot(HaveOccurred())
	Expect(tmpl.Execute(f, &e2e.BGPPeerValues{AS: GoBGPAS})).To(Succeed())
	f.Close()

	s.kill = make(chan any)
	s.done, err = runGoBGP(configPath, net.JoinHostPort(s.LocalIPv4, strconv.Itoa(int(GoBGPPort))), s.kill)
	Expect(err).ToNot(HaveOccurred())

	// Connect gRPC client (default: IPv4)
	s.Client, err = newGoBGPClient(s.LocalIPv4, GoBGPPort)
	Expect(err).ToNot(HaveOccurred())

	return s
}

// BuildServerFromInfo builds a Server from a local IPv4, local IPv6, and temp dir
// with a BGP client.
func BuildServerFromInfo(ipv4, ipv6, tempDir string) *Server {
	s := &Server{
		LocalIPv4: ipv4,
		LocalIPv6: ipv6,
		TempDir:   tempDir,
	}

	var err error
	s.Client, err = newGoBGPClient(ipv4, GoBGPPort)
	Expect(err).ToNot(HaveOccurred())
	return s
}

// NewClientIPv6 creates a second gRPC client connection via IPv6.
func (s *Server) NewClientIPv6() api.GoBgpServiceClient {
	c, err := newGoBGPClient(s.LocalIPv6, GoBGPPort)
	Expect(err).ToNot(HaveOccurred())
	return c
}

// Stop kills the GoBGP daemon.
func (s *Server) Stop() {
	if s.kill != nil {
		close(s.kill)
		s.kill = nil
	}
	if s.done != nil {
		Expect(<-s.done).To(Succeed())
		s.done = nil
	}
}

// ---------------------------------------------------------------------------
// Peer management — integration with Kind cluster nodes
// ---------------------------------------------------------------------------

// AddClusterPeers discovers container IPs for the given Kind nodes and adds
// them as BGP peers. Returns the peer list for later cleanup with RemovePeers.
func (s *Server) AddClusterPeers(ctx context.Context, clusterNodes []nodes.Node, peerAS uint32, families []string) []*e2e.BGPPeerValues {
	var peers []*e2e.BGPPeerValues

	for _, node := range clusterNodes {
		ipv4, ipv6, err := ContainerIPs(ctx, node.String())
		Expect(err).ToNot(HaveOccurred())

		for _, fam := range families {
			ip := ipv4
			if fam == "IPv6" {
				ip = ipv6
			}
			Expect(ip).ToNot(BeEmpty(), "container %s has no %s address", node.String(), fam)
			peers = append(peers, &e2e.BGPPeerValues{
				IP: ip, AS: peerAS, IPFamily: fam,
			})
		}
	}

	// Register peers with GoBGP
	for _, p := range peers {
		Eventually(func() error {
			_, err := s.Client.AddPeer(ctx, &api.AddPeerRequest{
				Peer: &api.Peer{
					Conf: &api.PeerConf{
						NeighborAddress: p.IP,
						PeerAsn:         uint32(p.AS),
					},
					AfiSafis: []*api.AfiSafi{
						{Config: &api.AfiSafiConfig{Enabled: true, Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}}},
						{Config: &api.AfiSafiConfig{Enabled: true, Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}}},
					},
				},
			})
			return err
		}, "120s", "100ms").Should(Succeed())
	}

	return peers
}

// RemovePeers deletes the given peers from GoBGP.
func (s *Server) RemovePeers(ctx context.Context, peers []*e2e.BGPPeerValues) {
	for _, p := range peers {
		Eventually(func() error {
			_, err := s.Client.DeletePeer(ctx, &api.DeletePeerRequest{Address: p.IP})
			return err
		}, "30s", "200ms").Should(Succeed())
	}
}

// ---------------------------------------------------------------------------
// VIP resolution via BGP
// ---------------------------------------------------------------------------

// ResolveVIP queries GoBGP for the current next-hops announcing the given VIP.
// Returns the node IPs that a real BGP router would forward traffic to.
func ResolveVIP(ctx context.Context, c api.GoBgpServiceClient, vip string) []string {
	family := &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
	dests, err := ListPaths(ctx, c, family, []*api.TableLookupPrefix{{Prefix: vip}})
	Expect(err).ToNot(HaveOccurred())

	var nexthops []string
	for _, d := range dests {
		for _, p := range d.Paths {
			if p.NeighborIp != "" {
				nexthops = append(nexthops, p.NeighborIp)
			}
		}
	}
	return nexthops
}

// ---------------------------------------------------------------------------
// Route verification
// ---------------------------------------------------------------------------

// CheckPaths polls GoBGP until exactly expectedDests destinations exist for
// the given prefix. Returns the destinations for further assertions.
// This counts Destination objects (used by the existing service BGP tests).
func CheckPaths(ctx context.Context, c api.GoBgpServiceClient, family *api.Family, prefixes []*api.TableLookupPrefix, expectedDests int) []*api.Destination {
	var paths []*api.Destination
	// 120s: with ginkgo --procs=4, sibling processes create Kind clusters and load
	// images concurrently, which can delay kube-vip's endpoint-watch/advertise loop
	// well past 30s (observed >13s even in passing specs). Matches the timeout the
	// BGP health-check suite uses for route re-announcement.
	Eventually(func() error {
		var err error
		paths, err = ListPaths(ctx, c, family, prefixes)
		if err != nil {
			return err
		}
		if len(paths) != expectedDests {
			return fmt.Errorf("expected %d destinations, found %d", expectedDests, len(paths))
		}
		return nil
	}, "120s", "1s").ShouldNot(HaveOccurred(), "should have %d destinations, found %d", expectedDests, len(paths))
	return paths
}

// CheckPathCount polls GoBGP until the total number of BGP paths (across all
// destinations) for the given prefix matches expected.
// This counts individual Path objects within Destinations (needed when multiple
// peers announce the same prefix, e.g. 2 CP nodes announcing the same VIP).
func CheckPathCount(ctx context.Context, c api.GoBgpServiceClient, prefix string, expected int, timeout time.Duration) {
	family := &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
	prefixes := []*api.TableLookupPrefix{{Prefix: prefix}}
	Eventually(func() error {
		dests, err := ListPaths(ctx, c, family, prefixes)
		if err != nil {
			return err
		}
		total := 0
		for _, d := range dests {
			total += len(d.Paths)
		}
		if total != expected {
			return fmt.Errorf("expected %d BGP paths, found %d", expected, total)
		}
		return nil
	}, timeout.String(), "2s").ShouldNot(HaveOccurred())
}

// ListPaths queries GoBGP for routes matching the given family and prefixes.
func ListPaths(ctx context.Context, c api.GoBgpServiceClient, family *api.Family, prefixes []*api.TableLookupPrefix) ([]*api.Destination, error) {
	pathCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stream, err := c.ListPath(pathCtx, &api.ListPathRequest{
		TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: family, Prefixes: prefixes,
		SortType: api.ListPathRequest_SORT_TYPE_PREFIX,
	})
	if err != nil {
		return nil, err
	}
	var rib []*api.Destination
	for {
		r, err := stream.Recv()
		if err != nil && err != io.EOF {
			return nil, err
		}
		if r != nil {
			rib = append(rib, r.Destination)
		}
		if err == io.EOF {
			break
		}
	}
	return rib, nil
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

// ContainerIPs returns the IPv4 and IPv6 addresses of a Docker container.
func ContainerIPs(ctx context.Context, containerName string) (string, string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return "", "", fmt.Errorf("failed to create docker client: %w", err)
	}
	containers, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to list containers: %w", err)
	}
	for _, c := range containers {
		for _, n := range c.Names {
			if n[1:] == containerName {
				for _, n := range c.NetworkSettings.Networks {
					return n.IPAddress, n.GlobalIPv6Address, nil
				}
			}
		}
	}
	return "", "", nil
}

// PeerStrings formats a list of BGPPeerValues as the comma-separated string
// expected by the bgp_peers env var.
func PeerStrings(peers []*e2e.BGPPeerValues) string {
	strs := make([]string, len(peers))
	for i, p := range peers {
		strs[i] = p.String()
	}
	return joinNonEmpty(strs)
}

func joinNonEmpty(ss []string) string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	result := ""
	for i, s := range out {
		if i > 0 {
			result += ","
		}
		result += s
	}
	return result
}

func newGoBGPClient(address string, port uint32) (api.GoBgpServiceClient, error) {
	target := net.JoinHostPort(address, strconv.Itoa(int(port)))
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to GoBGP server %q: %w", target, err)
	}
	return api.NewGoBgpServiceClient(conn), nil
}

func runGoBGP(config, address string, kill <-chan any) (chan error, error) {
	By("starting GoBGP server")
	cmd := exec.Command("../../bin/gobgpd", "--api-hosts", address, "-f", config)
	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start GoBGP: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err := waitForGoBGPReady(address, done, stdout, stderr, 15*time.Second); err != nil {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		default:
		}
		return nil, fmt.Errorf("start GoBGP on %s: %w", address, err)
	}

	stopped := make(chan error, 1)
	go func() {
		select {
		case err := <-done:
			stopped <- fmt.Errorf("GoBGP exited unexpectedly: %w%s", err, formatProcessLogs(stdout, stderr))
		case <-kill:
			By("stopping GoBGP server")
			if err := cmd.Process.Kill(); err != nil {
				stopped <- err
				return
			}
			<-done
			stopped <- nil
		}
	}()
	return stopped, nil
}

func waitForGoBGPReady(address string, exited <-chan error, stdout, stderr fmt.Stringer, timeout time.Duration) error {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create GoBGP API client: %w", err)
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-exited:
			if err == nil {
				return fmt.Errorf("process exited before becoming ready%s", formatProcessLogs(stdout, stderr))
			}
			return fmt.Errorf("process exited before becoming ready: %w%s", err, formatProcessLogs(stdout, stderr))
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for GoBGP API readiness%s", formatProcessLogs(stdout, stderr))
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			response, err := client.GetBgp(ctx, &api.GetBgpRequest{})
			cancel()
			if err == nil && response.GetGlobal().GetAsn() == GoBGPAS {
				return nil
			}
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func formatProcessLogs(stdout, stderr fmt.Stringer) string {
	var sections []string
	if output := strings.TrimSpace(stdout.String()); output != "" {
		sections = append(sections, "stdout:\n"+output)
	}
	if output := strings.TrimSpace(stderr.String()); output != "" {
		sections = append(sections, "stderr:\n"+output)
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n" + strings.Join(sections, "\n")
}
