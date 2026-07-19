package main

import (
	"os"
	"testing"
)

const priorStampJSON = `{"schema":1,"template_repo":"akamai-consulting/lke-landing-zone","template_ref":"v0.0.27"}`

// An upgrade that aborts on conflict markers must leave .template-version naming
// the ref the tree is ACTUALLY at. Stamping before the conflict gate and not
// rolling back left the instance claiming a ref it never reached, which every
// later upgrade and drift report then reads as its base.
func TestRestoreTemplateVersionFileRollsBack(t *testing.T) {
	chdirTemp(t)
	writeFile(t, ".template-version", priorStampJSON)

	prior, had := readTemplateVersionFile()
	if !had {
		t.Fatal("readTemplateVersionFile: want had=true for an existing stamp")
	}

	// Simulate the stamp the upgrade writes before the gate fires.
	writeFile(t, ".template-version", `{"schema":1,"template_ref":"v0.0.28"}`)
	restoreTemplateVersionFile(prior, had)

	got, err := os.ReadFile(".template-version")
	if err != nil {
		t.Fatalf("read after restore: %v", err)
	}
	if string(got) != priorStampJSON {
		t.Errorf("stamp not rolled back byte-for-byte\n got: %s\nwant: %s", got, priorStampJSON)
	}
}

// A pre-stamp instance had no file; an aborted upgrade must not leave one behind.
func TestRestoreTemplateVersionFileRemovesWhenAbsent(t *testing.T) {
	chdirTemp(t)

	prior, had := readTemplateVersionFile()
	if had {
		t.Fatal("readTemplateVersionFile: want had=false when no stamp exists")
	}

	writeFile(t, ".template-version", `{"schema":1,"template_ref":"v0.0.28"}`)
	restoreTemplateVersionFile(prior, had)

	if _, err := os.Stat(".template-version"); !os.IsNotExist(err) {
		t.Errorf("stamp must be removed again when the instance had none; stat err = %v", err)
	}
}

// Restoring twice (or with nothing to remove) must not panic or error-spam.
func TestRestoreTemplateVersionFileIdempotent(t *testing.T) {
	chdirTemp(t)

	prior, had := readTemplateVersionFile()
	restoreTemplateVersionFile(prior, had)
	restoreTemplateVersionFile(prior, had)

	if _, err := os.Stat(".template-version"); !os.IsNotExist(err) {
		t.Errorf("repeated restore must stay a no-op; stat err = %v", err)
	}
}
