//go:build e2e
// +build e2e

package e2e_test

import (
	"log"
	"os"
	"sync"
	"testing"

	"github.com/kube-vip/kube-vip/testing/e2e"
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

var _ = SynchronizedBeforeSuite(
	func() {
		e2e.EnsureKindNetwork()
	},
	func() {},
)
