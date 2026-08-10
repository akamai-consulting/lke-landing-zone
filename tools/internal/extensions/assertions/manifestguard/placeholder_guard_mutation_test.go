package manifestguard

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// Rendered charts concatenate every template, so a single manifest can carry a
// line far longer than bufio.Scanner's 64KB default token limit. The guard
// raises that limit on purpose: without the headroom the scan errors out and
// the guard fails the build with a scanner error instead of reporting on the
// tree — and a placeholder in a long-lined file is never seen at all.
func TestCollectPlaceholderFindingsScansLinesPastTheScannerDefault(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", 200*1024) // ~200KB on one line, no newline inside
	writeManifest(t, dir, "big.yaml", "host: "+placeholderHost+"\ndata: "+long+"\ntrailer: ok\n")

	findings, examined, err := collectPlaceholderFindings(capability.RepoAt(renderedManifestsBinding(), dir), []string{"."})
	if err != nil {
		t.Fatalf("a %d-byte line must fit the scan buffer, got %v", len(long), err)
	}
	if examined != 1 {
		t.Errorf("examined = %d, want 1", examined)
	}
	if len(findings) != 1 || findings[0].line != 1 {
		t.Fatalf("findings = %+v, want the placeholder on line 1", findings)
	}
}
