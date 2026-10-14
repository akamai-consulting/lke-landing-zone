package atrest

// corpus_property_test.go — the stripper, checked against the repo's REAL
// Terraform rather than against fixtures it was written alongside.
//
// stripHCL is a hand-rolled byte scanner, and a bug in it is a SILENT FALSE
// NEGATIVE on a security gate: the depth walk runs past the end of a resource,
// `i = j - 1` skips everything below, and the guard reports green having never
// looked. There is no symptom to notice. Table tests cannot find that class
// because they only contain the shapes their author already thought of — the
// apostrophe-in-a-comment case that shipped is proof.
//
// The invariant is HCL's own and needs no oracle: a syntactically valid
// Terraform file has BALANCED BRACES, so summing braceDelta over the denoised
// lines of a whole file must be exactly 0. Any comment, string or block-comment
// mishandling that swallows or invents a brace shows up here as a non-zero total
// on a file that really is balanced. `terraform fmt` gates this corpus in CI, so
// "the file is valid" is an assumption the repo already enforces.
//
// MEASURED, AND THE NUMBER IS WHY THIS FILE IS WORTH ITS RUNTIME. Against the
// corpus as it stands today the OLD stripper passes too — no .tf here currently
// contains an apostrophe in a comment, so the shipped bug was LATENT, not
// active. Inject one ordinary `/* don't do this */` into each file and the old
// stripper goes unbalanced on 21 of the 37 tracked files; the new one on none.
// The gate was one English contraction away from going blind across half the
// repo's Terraform, and no fixture-based test would have said so.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minTFCorpus is the vacuity floor: enough to prove the walk found the repo, far
// enough below the real count (~37 tracked) that removing a root does not fail
// this test for the wrong reason.
const minTFCorpus = 20

// repoTFFiles returns every .tf file in the repository.
func repoTFFiles(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../../../../..")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	err = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable subtree: not this test's business
		}
		if fi.IsDir() {
			// EVERY dot-directory, not a named list. `.tf-instance/` — the scaffold a
			// local `make` run leaves behind — carries 48 more .tf files than a clean
			// checkout has, so a floor calibrated against a developer's worktree fails
			// in CI on a corpus that is perfectly fine. Tracked source does not live
			// in a dot-directory; generated and scratch trees do.
			if strings.HasPrefix(fi.Name(), ".") || fi.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".tf") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStripHCLKeepsEveryRealTerraformFileBalanced(t *testing.T) {
	files := repoTFFiles(t)
	// FAIL CLOSED ON AN EMPTY CORPUS. A property test that walked no files is
	// indistinguishable from one that passed, and this walk is relative to the
	// package directory — one move and it silently proves nothing.
	//
	// The floor is well under the ~37 files a clean checkout tracks, deliberately:
	// its job is to catch "the walk found nothing / went to the wrong place", not
	// to restate the corpus size. A first cut set it at 50, measured against a
	// worktree that also held a generated `.tf-instance/` scaffold, and failed CI
	// on a corpus that was entirely correct.
	if len(files) < minTFCorpus {
		t.Fatalf("found only %d .tf files (want >= %d) — this test is looking in the wrong place "+
			"and would pass having examined nothing", len(files), minTFCorpus)
	}

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		_, noise := stripHCL(strings.Split(string(b), "\n"))
		depth := 0
		for _, l := range noise {
			depth += braceDelta(l)
		}
		if depth != 0 {
			t.Errorf("%s: denoised brace depth = %d, want 0. A valid .tf file is balanced, so the "+
				"stripper swallowed or invented a brace — which makes the depth walk run past the end "+
				"of a resource and silently skip every resource below it",
				mustRel(t, f), depth)
		}
	}
	t.Logf("checked %d real Terraform files", len(files))
}

// TestStripHCLNeverGrowsALine — the containment property. Stripping only ever
// removes, so each view must be no longer than the one above it. A scanner bug
// that duplicated a byte (an off-by-one on the `j` advance past `*/`) would
// otherwise be invisible in the balance check.
func TestStripHCLNeverGrowsALine(t *testing.T) {
	files := repoTFFiles(t)
	if len(files) < minTFCorpus {
		t.Fatalf("found only %d .tf files (want >= %d) — wrong place", len(files), minTFCorpus)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		decommented, noise := stripHCL(lines)
		for i := range lines {
			if len(decommented[i]) > len(lines[i]) {
				t.Errorf("%s:%d: decommented (%d bytes) is LONGER than the source line (%d) — the "+
					"scanner emitted bytes it did not read:\n  src: %q\n  got: %q",
					mustRel(t, f), i+1, len(decommented[i]), len(lines[i]), lines[i], decommented[i])
			}
			if len(noise[i]) > len(decommented[i]) {
				t.Errorf("%s:%d: noise (%d bytes) is longer than decommented (%d) — noise strips "+
					"strictly more", mustRel(t, f), i+1, len(noise[i]), len(decommented[i]))
			}
		}
	}
}

func mustRel(t *testing.T, p string) string {
	t.Helper()
	root, err := filepath.Abs("../../../../..")
	if err != nil {
		return p
	}
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return r
}
