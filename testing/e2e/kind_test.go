//go:build e2e
// +build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/exec"
)

func TestLoadDockerImageToKindSaveFailure(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  "image save")
    if [ "$3" != "--platform" ] || [ "$4" != "$EXPECTED_PLATFORM" ]; then
      echo "wrong export platform" >&2
      exit 2
    fi
    echo "missing blob" >&2
    exit 1
    ;;
  *)
    echo "unexpected command" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("EXPECTED_PLATFORM", "linux/"+runtime.GOARCH)

	err := LoadDockerImageToKind(TestLogger{}, "example.com/test:v1", "test")
	var runErr *exec.RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("expected command error, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed to save image archive") || !strings.Contains(string(runErr.Output), "missing blob") {
		t.Fatalf("expected export error with preserved stderr, got %v (output %q)", err, runErr.Output)
	}
}

func TestPrintCommandOutputIfErr(t *testing.T) {
	runErr := &exec.RunError{
		Command: []string{"ctr", "images", "import"},
		Output:  []byte("content digest not found"),
		Inner:   errors.New("exit status 1"),
	}
	for _, err := range []error{nil, errors.New("plain error"), fmt.Errorf("wrapped: %w", runErr)} {
		if got := PrintCommandOutputIfErr(err); got != err {
			t.Fatalf("returned %v, want original error %v", got, err)
		}
	}
}
