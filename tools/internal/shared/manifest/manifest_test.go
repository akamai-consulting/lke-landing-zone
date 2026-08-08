package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateManifestCheckClassifyAndList(t *testing.T) {
	root := t.TempDir()
	scaffold := filepath.Join(root, "instance-template")
	writeTestFile(t, scaffold, ".template-manifest", ""+
		"managed **\n"+
		"merge docs/**\n"+
		"owned docs/local.md\n")
	writeTestFile(t, scaffold, "README.md", "readme\n")
	writeTestFile(t, scaffold, "docs/guide.md", "guide\n")
	writeTestFile(t, scaffold, "docs/local.md", "local\n")
	writeTestFile(t, filepath.Join(scaffold, ".terraform"), "ignored.tf", "ignored\n")

	var out, errOut bytes.Buffer
	if err := Run(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("check failed: %v\nstderr: %s", err, errOut.String())
	}
	for _, want := range []string{"managed=2", "merge=1", "owned=1", "4 files"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check output %q missing %q", out.String(), want)
		}
	}

	out.Reset()
	errOut.Reset()
	if err := Run(scaffold, "docs/local.md", "", &out, &errOut); err != nil {
		t.Fatalf("classify failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "owned" {
		t.Fatalf("classify = %q, want owned", got)
	}

	out.Reset()
	if err := Run(scaffold, "", "merge", &out, &errOut); err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "docs/guide.md" {
		t.Fatalf("list merge = %q, want docs/guide.md", got)
	}
}

func TestTemplateManifestReportsUnclassifiedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".template-manifest", "managed .template-manifest\nmanaged README.md\n")
	writeTestFile(t, root, "README.md", "readme\n")
	writeTestFile(t, root, "values.yaml", "x\n")

	var out, errOut bytes.Buffer
	err := Run(root, "", "", &out, &errOut)
	if err == nil {
		t.Fatal("expected unclassified-file error")
	}
	for _, want := range []string{"::error::1 scaffold file", "values.yaml"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q missing %q", errOut.String(), want)
		}
	}
}

func TestTemplateManifestCopierConsistency(t *testing.T) {
	root := t.TempDir()
	scaffold := filepath.Join(root, "instance-template")
	writeTestFile(t, scaffold, ".template-manifest", ""+
		"managed **\n"+
		"owned keep.txt\n"+
		"owned .copier-answers.yml\n")
	writeTestFile(t, scaffold, "keep.txt", "instance content\n")
	writeTestFile(t, scaffold, ".copier-answers.yml", "_commit: v1\n")

	// copier.yml protecting NEITHER owned file: keep.txt is a violation, but
	// .copier-answers.yml is exempt (it is the _answers_file copier regenerates).
	writeTestFile(t, root, "copier.yml", "_answers_file: .copier-answers.yml\n_skip_if_exists: []\n_exclude: []\n")

	var out, errOut bytes.Buffer
	if err := Run(scaffold, "", "", &out, &errOut); err == nil {
		t.Fatalf("expected failure: keep.txt is owned but unprotected by copier\nstdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "keep.txt") {
		t.Errorf("error should name the unprotected owned file: %s", errOut.String())
	}
	if strings.Contains(errOut.String(), ".copier-answers.yml") {
		t.Errorf("_answers_file must be exempt from the check: %s", errOut.String())
	}

	// Protect keep.txt via _skip_if_exists → now consistent.
	writeTestFile(t, root, "copier.yml", "_answers_file: .copier-answers.yml\n_skip_if_exists:\n  - \"keep.txt\"\n")
	out.Reset()
	errOut.Reset()
	if err := Run(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("should pass once keep.txt is protected: %v\nstderr: %s", err, errOut.String())
	}

	// _exclude also counts as protection.
	writeTestFile(t, root, "copier.yml", "_answers_file: .copier-answers.yml\n_exclude:\n  - \"keep.txt\"\n")
	out.Reset()
	errOut.Reset()
	if err := Run(scaffold, "", "", &out, &errOut); err != nil {
		t.Fatalf("_exclude should also satisfy the check: %v\nstderr: %s", err, errOut.String())
	}
}

// The class table is the single authority the manifest, copier.yml and the digest
// lock all read. These assertions pin the invariants a new row must preserve, so
// adding a class cannot quietly leave one of the three consumers behind.
func TestTemplateClassTableInvariants(t *testing.T) {
	if len(templateClasses) == 0 {
		t.Fatal("templateClasses must not be empty")
	}
	seen := map[string]bool{}
	for _, c := range templateClasses {
		if c.name == "" {
			t.Error("every class needs a name")
		}
		if seen[c.name] {
			t.Errorf("duplicate class %q — classify() is last-match-wins over names, so they must be unique", c.name)
		}
		seen[c.name] = true
		if c.summary == "" {
			t.Errorf("class %q has no summary — the manifest header documents each class", c.name)
		}
		switch c.Upgrade {
		case UpgradeOverwrite, upgradeMerge, UpgradeRestore:
		default:
			t.Errorf("class %q has unknown upgrade action %q", c.name, c.Upgrade)
		}
		// The digest lock exists because an upgrade DISCARDS the instance's bytes.
		// Locking a class the upgrade preserves would fail on every legitimate edit.
		if c.DigestLocked && c.Upgrade != UpgradeOverwrite {
			t.Errorf("class %q is digestLocked but its upgrade action is %q — only overwritten classes may be locked",
				c.name, c.Upgrade)
		}
		// Fencing copier off a file only makes sense when the instance owns it;
		// a class the template overwrites has nothing to protect.
		if c.copierFenced && c.Upgrade == UpgradeOverwrite {
			t.Errorf("class %q is copierFenced but overwritten on upgrade — the fence would be pointless", c.name)
		}
	}
	if !ValidClass("managed") || ValidClass("nonsense") {
		t.Error("ValidClass must be backed by the table")
	}
	if got := ClassNames(); !strings.Contains(got, "managed") || !strings.Contains(got, "owned") {
		t.Errorf("ClassNames() = %q, want it to list the table's names", got)
	}
}

// checkCopierFencing must read the table's copierFenced flag, not a hardcoded
// "owned" — otherwise a future fenced class (the extension/recipe class in #15)
// silently gets no copier protection at all.
func TestCopierFencingIsDrivenByTheTable(t *testing.T) {
	var fenced []string
	for _, c := range templateClasses {
		if c.copierFenced {
			fenced = append(fenced, c.name)
		}
	}
	if len(fenced) == 0 {
		t.Skip("no copier-fenced class in the table")
	}
	for _, class := range fenced {
		root := t.TempDir()
		scaffold := filepath.Join(root, "instance-template")
		writeTestFile(t, scaffold, ".template-manifest", "managed **\n"+class+" keep.txt\n")
		writeTestFile(t, scaffold, "keep.txt", "instance content\n")
		writeTestFile(t, root, "copier.yml", "_skip_if_exists: []\n_exclude: []\n")

		var out, errOut bytes.Buffer
		if err := Run(scaffold, "", "", &out, &errOut); err == nil {
			t.Errorf("class %q is copierFenced but an unprotected file passed the check\nstdout: %s",
				class, out.String())
		}
	}
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── reverse containment (ADR 0014) ───────────────────────────────────────────
//
// The forward check asks "is every fenced file protected by copier?". These
// cover the converse: "is every file copier skips one the manifest says the
// instance owns?" A `managed`/`merge` file in `_skip_if_exists` receives no
// update from copier and no restore from `llz upgrade` — it silently freezes.

// reverseFixture lays out a scaffold whose single file `keep.txt` has the given
// manifest class and the given copier protection.
func reverseFixture(t *testing.T, class, copierYML string) (scaffold string, out, errOut *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	scaffold = filepath.Join(root, "instance-template")
	writeTestFile(t, scaffold, ".template-manifest", "managed **\n"+class+" keep.txt\n")
	writeTestFile(t, scaffold, "keep.txt", "content\n")
	writeTestFile(t, root, "copier.yml", copierYML)
	return scaffold, &bytes.Buffer{}, &bytes.Buffer{}
}

func TestCopierFencingReverseRejectsFencingAnUnownedFile(t *testing.T) {
	for _, class := range []string{"managed", "merge"} {
		t.Run(class, func(t *testing.T) {
			scaffold, out, errOut := reverseFixture(t, class,
				"_skip_if_exists:\n  - \"keep.txt\"\n_exclude: []\n")
			err := Run(scaffold, "", "", out, errOut)
			if err == nil {
				t.Fatalf("a %s file fenced by _skip_if_exists must fail\nstdout: %s", class, out.String())
			}
			if !strings.Contains(errOut.String(), "keep.txt") {
				t.Errorf("the error must name the offending file, got: %s", errOut.String())
			}
		})
	}
}

func TestCopierFencingReverseAcceptsAnOwnedFile(t *testing.T) {
	scaffold, out, errOut := reverseFixture(t, "owned",
		"_skip_if_exists:\n  - \"keep.txt\"\n_exclude: []\n")
	if err := Run(scaffold, "", "", out, errOut); err != nil {
		t.Fatalf("an owned file fenced by _skip_if_exists is the correct pairing: %v\nstderr: %s", err, errOut.String())
	}
}

// `_exclude` is a DELIVERY decision (the file never ships), not an ownership
// claim, so it must not drag a template-owned file into the fenced classes.
func TestCopierFencingReverseIgnoresExclude(t *testing.T) {
	scaffold, out, errOut := reverseFixture(t, "managed",
		"_skip_if_exists: []\n_exclude:\n  - \"keep.txt\"\n")
	if err := Run(scaffold, "", "", out, errOut); err != nil {
		t.Fatalf("_exclude must not be treated as an ownership claim: %v\nstderr: %s", err, errOut.String())
	}
}

// An unmatched fence rule is dead config, but it may legitimately anticipate a
// file the template does not ship yet — so it is reported, not enforced.
func TestCopierFencingReverseNotesUnmatchedRuleWithoutFailing(t *testing.T) {
	scaffold, out, errOut := reverseFixture(t, "owned",
		"_skip_if_exists:\n  - \"keep.txt\"\n  - \"never/shipped.txt\"\n_exclude: []\n")
	if err := Run(scaffold, "", "", out, errOut); err != nil {
		t.Fatalf("an unmatched fence rule must not fail the gate: %v", err)
	}
	if !strings.Contains(errOut.String(), "never/shipped.txt") {
		t.Errorf("the unmatched rule should be reported, got: %s", errOut.String())
	}
}

// fencedClassNames is derived from the table so a new fenced class needs no edit
// in the error text — the same property TestCopierFencingIsDrivenByTheTable
// pins for the check itself.
func TestFencedClassNamesComesFromTheTable(t *testing.T) {
	got := fencedClassNames()
	for _, c := range templateClasses {
		if c.copierFenced && !strings.Contains(got, c.name) {
			t.Errorf("fencedClassNames() = %q, missing fenced class %q", got, c.name)
		}
		if !c.copierFenced && strings.Contains(got, c.name) {
			t.Errorf("fencedClassNames() = %q, lists unfenced class %q", got, c.name)
		}
	}
}
