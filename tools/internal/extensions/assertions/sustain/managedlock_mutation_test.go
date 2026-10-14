package sustain

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadManagedLockNamesTheOffendingLineNumber(t *testing.T) {
	p := filepath.Join(t.TempDir(), ManagedLockPath)
	writeFile(t, p, "# GENERATED — do not hand-edit\nnot-a-valid-entry\n")

	_, err := ReadManagedLock(p)
	if err == nil {
		t.Fatal("a malformed entry must be rejected")
	}
	if !strings.Contains(err.Error(), ":2 bad entry") {
		t.Errorf("error must name line 2, got %v", err)
	}
}
