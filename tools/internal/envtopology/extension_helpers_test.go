package envtopology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nonTestSources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no non-test sources — the scan is looking in the wrong directory, " +
			"and an empty scan passes every assertion over it")
	}
	return out
}

func containsCall(t *testing.T, file, call string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), call+"(")
}
