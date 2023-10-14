//go:build e2e
// +build e2e

package etcd_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestEtcd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Etcd Suite")
}
