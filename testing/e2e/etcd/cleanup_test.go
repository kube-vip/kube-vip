//go:build e2e
// +build e2e

package etcd_test

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

func TestCleanupWithoutCluster(t *testing.T) {
	RegisterTestingT(t)
	t.Setenv("E2E_PRESERVE_CLUSTER", "false")

	test := &testConfig{
		kubeVipManifestPath: filepath.Join(t.TempDir(), "missing-manifest"),
		etcdCertsFolder:     filepath.Join(t.TempDir(), "missing-certs"),
	}

	test.cleanup()
}
