package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "admin.conf")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "regular file", path: file, want: true},
		{name: "directory", path: dir, want: false},
		{name: "missing", path: filepath.Join(dir, "missing.conf"), want: false},
		{name: "parent is a file", path: filepath.Join(file, "admin.conf"), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FileExists(test.path); got != test.want {
				t.Fatalf("FileExists(%q) = %t, want %t", test.path, got, test.want)
			}
		})
	}
}
