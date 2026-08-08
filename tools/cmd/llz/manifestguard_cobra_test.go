package main

// manifestguard_cobra_test.go — the guard tests that assert against the LIVE
// cobra tree, and therefore stayed.
//
// Same reason six of internal/docsguard's tests stayed: only package main can
// build the command tree, so a test that runs the verb through its flag set has to
// live here even though the logic it exercises does not. Note the coverage
// consequence — `go test -coverprofile` credits these to package main, so a low
// per-package number on a freshly extracted guard means "its tests are elsewhere"
// before it means "it is untested".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
)

// An absolute --render-dir must survive the --root join: filepath.Join(".",
// "/abs") cleans to "abs", which would silently retarget the scan at a relative
// path that does not exist and surface as a bogus empty-corpus failure.
func TestPlaceholderGuardAcceptsAbsoluteRenderDir(t *testing.T) {
	dir := t.TempDir() // t.TempDir() is absolute
	writeManifest(t, dir, "c.yaml", "host: placeholder.example.com\n")
	cmd := manifestguard.PlaceholderGuardCmd()
	cmd.SetArgs([]string{"--render-dir", dir})
	if err := cmd.Execute(); err == nil {
		t.Fatal("absolute --render-dir should have been scanned and found the placeholder")
	} else if strings.Contains(err.Error(), "examined 0") {
		t.Fatalf("absolute path was mangled by the --root join: %v", err)
	}
}

func writeManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
