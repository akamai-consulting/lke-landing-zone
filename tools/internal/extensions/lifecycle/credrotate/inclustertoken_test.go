package credrotate

// inclustertoken_test.go — moved out of the es-store-recovery lane's test file when
// that lane was extracted. It tests linode_token.go, which stayed: the reconciler
// reads its Linode token lazily from an OPTIONAL Secret volume, so "the file
// appears later" is the case that matters and the one this pins.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

func TestInclusterLinodeToken(t *testing.T) {
	dir := t.TempDir()
	prev := linode.LinodeTokenFile
	linode.LinodeTokenFile = filepath.Join(dir, "token")
	t.Cleanup(func() { linode.LinodeTokenFile = prev })

	// Neither env nor file → empty.
	t.Setenv("LINODE_TOKEN", "")
	if got := linode.InClusterLinodeToken(); got != "" {
		t.Fatalf("no source: got %q", got)
	}
	// File appears (the optional volume materializing) → picked up lazily.
	if err := os.WriteFile(linode.LinodeTokenFile, []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := linode.InClusterLinodeToken(); got != "file-tok" {
		t.Fatalf("file source: got %q", got)
	}
	// Env wins (CronJob/CI compatibility).
	t.Setenv("LINODE_TOKEN", "env-tok")
	if got := linode.InClusterLinodeToken(); got != "env-tok" {
		t.Fatalf("env precedence: got %q", got)
	}
}
