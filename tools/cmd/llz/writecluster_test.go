package main

// A COPY of the writeCluster fixture, which travelled to internal/envtopology with
// envlist_test.go while render_test.go still needs it.

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCluster seeds <dir>/cluster/<name>.tfvars files for the discovery tests.
func writeCluster(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "cluster"), 0o755); err != nil {
		t.Fatalf("mkdir cluster: %v", err)
	}
	for name, body := range files {
		mustWrite(t, filepath.Join(dir, "cluster", name), body)
	}
}
