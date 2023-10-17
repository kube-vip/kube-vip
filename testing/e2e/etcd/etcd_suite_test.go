//go:build e2e
// +build e2e

package etcd_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/testing/e2e"
)

func TestEtcd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Etcd Suite")
}

var _ = SynchronizedBeforeSuite(
	func() {
		e2e.EnsureKindNetwork()
	},
	func() {},
)
