//go:build e2e
// +build e2e

package e2e_test

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/testing/e2e"
	"github.com/kube-vip/kube-vip/testing/e2e/bgp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	ModeEnv = "TEST_MODE"
	ModeARP = "arp"
	ModeRT  = "rt"
	ModeBGP = "bgp"
)

var (
	SOffset   *e2e.SecureOffset
	ConfigMtx *sync.Mutex
	Mode      string

	// sharedBGPServer is a GoBGP server shared across all parallel Ginkgo
	// processes in BGP mode. Process 1 starts the daemon; other processes
	// connect to it. Nil in non-BGP modes.
	sharedBGPServer *bgp.Server

	// originalBGPServer is the original GoBGP server that was started by Process 1.
	// It is used to close the server when the suite is finished.
	originalBGPServer *bgp.Server
)

func TestE2E(t *testing.T) {
	mode := os.Getenv(ModeEnv)
	if mode == "" {
		Mode = ModeARP
	} else if mode != ModeARP && mode != ModeRT && mode != ModeBGP {
		log.Fatal("invalid", "mode", mode)
		os.Exit(1)
	} else {
		Mode = mode
	}
	SOffset = e2e.NewOffset(5)
	ConfigMtx = &sync.Mutex{}
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

// bgpServerInfo is serialized as JSON between Ginkgo processes.
type bgpServerInfo struct {
	LocalIPv4   string `json:"localIPv4"`
	LocalIPv6   string `json:"localIPv6"`
	TempDirPath string `json:"tempDirPath"`
}

var _ = SynchronizedBeforeSuite(
	// Process 1: ensure Kind network, start GoBGP if in BGP mode.
	func() []byte {
		e2e.EnsureKindNetwork()

		if Mode != ModeBGP {
			return nil
		}

		tmpDir, err := os.MkdirTemp("", "kube-vip-bgp-shared")
		Expect(err).NotTo(HaveOccurred())

		srv := bgp.NewServer(tmpDir)

		data, err := json.Marshal(bgpServerInfo{
			LocalIPv4:   srv.LocalIPv4,
			LocalIPv6:   srv.LocalIPv6,
			TempDirPath: tmpDir,
		})
		Expect(err).NotTo(HaveOccurred())

		originalBGPServer = srv
		return data
	},
	// All processes: connect to the shared GoBGP server.
	func(data []byte) {
		if Mode != ModeBGP || len(data) == 0 {
			return
		}

		var info bgpServerInfo
		Expect(json.Unmarshal(data, &info)).To(Succeed())

		sharedBGPServer = bgp.BuildServerFromInfo(info.LocalIPv4, info.LocalIPv6, info.TempDirPath)
	},
)

var _ = SynchronizedAfterSuite(
	// All processes: no-op.
	func() {},
	// Process 1 only: stop GoBGP and clean up temp dir.
	func() {
		if originalBGPServer != nil {
			originalBGPServer.Stop()
		}
	},
)
