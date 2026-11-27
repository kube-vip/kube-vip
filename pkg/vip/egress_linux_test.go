//go:build linux

package vip

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kube-vip/kube-vip/pkg/iptables"
)

// TestClearIPTablesReturnsCleanError uses a fake iptables binary that reports a
// version but fails every other command, so the cleanup itself fails.
func TestClearIPTablesReturnsCleanError(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  echo \"iptables v1.8.9 (legacy)\"\n  exit 0\nfi\necho \"iptables: cannot list rules\" >&2\nexit 2\n"
	if err := os.WriteFile(filepath.Join(dir, "iptables-legacy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := ClearIPTables(false, "default", iptables.ProtocolIPv4); err == nil {
		t.Fatal("ClearIPTables() returned nil although the cleanup failed")
	}
}
