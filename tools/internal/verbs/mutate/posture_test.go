package mutate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaysFilesOnly(t *testing.T) {
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
		for _, bad := range []string{"net/http", "k8s.io/", "linodego", "kubectlprobe"} {
			if strings.Contains(string(b), `"`+bad) {
				t.Errorf("%s reaches %s — a gate may hold read-repo and nothing else", f, bad)
			}
		}
	}
}
