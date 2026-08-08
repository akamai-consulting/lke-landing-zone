package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCrossOrgGateStaysFilesOnly(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".", "crossorg.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"net/http", "os/exec", "linodego", "kubectlprobe"} {
		if strings.Contains(string(b), `"`+bad) || strings.Contains(string(b), bad+`"`) {
			t.Errorf("crossorg.go reaches %s — it backs the GATE binding, which may hold read-repo alone", bad)
		}
	}
}
