package argodiag_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPackageStaysReadOnly(t *testing.T) {
	mutating := regexp.MustCompile(`"(apply|patch|delete|create|replace|annotate|label|scale|edit|rollout|cordon|drain|taint)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := mutating.FindString(string(b)); m != "" {
			t.Errorf("%s passes a mutating kubectl verb (%s) — the declaration says cluster-read only", f, m)
		}
		if strings.Contains(string(b), "os.WriteFile") || strings.Contains(string(b), "os.RemoveAll") {
			t.Errorf("%s writes to disk — the declaration claims no write grant", f)
		}
	}
}
