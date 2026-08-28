package manifestguard

// It reads droppedAPIHit.loc, an unexported field, so it could only ever live in
// this package — which is where its subject now is.

import (
	"os"
	"path/filepath"
	"testing"
)

// The report is a `file:line` an operator (and the ::error annotation) jumps to.
// An off-by-one sends them to the wrong line — or to line 0/-1, which no editor
// or PR annotation can resolve at all.
func TestScanDroppedAPIVersionsReportsTheDeclaringLine(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("kubernetes-custom", "es.yaml")
	if err := os.MkdirAll(filepath.Join(root, "kubernetes-custom"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The dropped apiVersion is on line 3 (1-based).
	body := "# a comment\n---\napiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n"
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, examined, err := ScanDroppedAPIVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	if examined == 0 {
		t.Fatal("examined = 0: the scan read nothing, so a clean result would be meaningless")
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly one", hits)
	}
	if want := "kubernetes-custom/es.yaml:3"; hits[0].loc != want {
		t.Errorf("hit loc = %q, want %q (1-based line of the declaration)", hits[0].loc, want)
	}
}
