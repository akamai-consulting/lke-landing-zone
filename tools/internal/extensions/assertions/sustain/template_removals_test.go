package sustain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTemplateRemovals(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".template-removals")
	writeFile(t, p, `# header comment
untrack  terraform-iac-bootstrap/*/[a-z]*.tfvars

delete   platform-apl/manifest/dns/old-webhook.yaml
`)
	rules, err := readTemplateRemovals(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d: %+v", len(rules), rules)
	}
	if rules[0].mode != "untrack" || rules[1].mode != "delete" {
		t.Errorf("modes: %+v", rules)
	}

	// Missing file is not an error — an older instance has nothing to remove.
	if r, err := readTemplateRemovals(filepath.Join(dir, "nope")); err != nil || r != nil {
		t.Errorf("missing file: got (%v, %v), want (nil, nil)", r, err)
	}

	// Malformed lines are rejected loudly.
	bad := filepath.Join(dir, "bad")
	for _, line := range []string{"untrack", "sideways  a/b", "untrack a b c"} {
		writeFile(t, bad, line+"\n")
		if _, err := readTemplateRemovals(bad); err == nil {
			t.Errorf("expected error for %q", line)
		}
	}
}

// applyTemplateRemovals must untrack (keep on disk) and delete (remove from disk)
// the right git-tracked files, leave everything else, and be idempotent.
func TestApplyTemplateRemovals(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	dir := t.TempDir()
	tracked := []string{
		"terraform-iac-bootstrap/cluster/lab.tfvars",               // untrack
		"terraform-iac-bootstrap/object-storage/lab.tfvars",        // untrack
		"terraform-iac-bootstrap/cluster/terraform.tfvars.example", // keep (.example)
		"terraform-iac-bootstrap/cluster/main.tf",                  // keep
		"platform-apl/manifest/dns/old-webhook.yaml",               // delete
	}
	for _, p := range tracked {
		writeFile(t, filepath.Join(dir, p), "x\n")
	}
	writeFile(t, filepath.Join(dir, ".template-removals"), `untrack  terraform-iac-bootstrap/*/[a-z]*.tfvars
delete   platform-apl/manifest/dns/old-webhook.yaml
`)
	gitInitRepo(t, dir, append(tracked, ".template-removals")...)
	chdir(t, dir)

	if err := ApplyTemplateRemovals(realGitDeps(t)); err != nil {
		t.Fatal(err)
	}

	got := gitTracked(t, dir)
	want := []string{
		".template-removals",
		"terraform-iac-bootstrap/cluster/main.tf",
		"terraform-iac-bootstrap/cluster/terraform.tfvars.example",
	}
	if join(got) != join(want) {
		t.Errorf("tracked after removals\n got: %v\nwant: %v", got, want)
	}
	// untrack KEEPS the file on disk; delete removes it.
	if _, err := os.Stat(filepath.Join(dir, "terraform-iac-bootstrap/cluster/lab.tfvars")); err != nil {
		t.Errorf("untrack must keep the file on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "platform-apl/manifest/dns/old-webhook.yaml")); !os.IsNotExist(err) {
		t.Errorf("delete must remove the file from disk; stat err = %v", err)
	}

	// Idempotent: a second pass changes nothing.
	if err := ApplyTemplateRemovals(realGitDeps(t)); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if join(gitTracked(t, dir)) != join(want) {
		t.Error("second applyTemplateRemovals was not a no-op")
	}
}

func TestApplyTemplateRemovals_NoFileNoOp(t *testing.T) {
	dir := t.TempDir()
	p := "terraform-iac-bootstrap/cluster/lab.tfvars"
	writeFile(t, filepath.Join(dir, p), "x\n")
	gitInitRepo(t, dir, p)
	chdir(t, dir)
	if err := ApplyTemplateRemovals(realGitDeps(t)); err != nil {
		t.Fatal(err)
	}
	if got := gitTracked(t, dir); len(got) != 1 || got[0] != p {
		t.Errorf("no .template-removals should be a no-op; tracked: %v", got)
	}
}

func join(s []string) string {
	out := ""
	for _, x := range s {
		out += x + "\n"
	}
	return out
}

// shippedRemovals is instance-template/.template-removals — the real file, not a
// fixture. The tests above prove the MECHANISM on synthetic rules; this one
// proves the rules actually shipped hit the paths they name.
const shippedRemovals = "../../../../../instance-template/.template-removals"

// TestTheShippedRemovalRulesMatchRealInstancePaths.
//
// A removal rule that matches nothing is a SILENT no-op: `llz upgrade` applies it,
// finds no tracked file, reports nothing, and every instance in the field keeps
// the artifact the template meant to take away. Nothing else in the tree looks at
// these globs, so a typo — or the difference between filepath.Match (where `*`
// stops at a '/') and the shell globbing a reader has in mind — costs the entire
// migration with no failure anywhere.
//
// THE PROVIDER LOCK IS WHY THIS EXISTS. Its rule is the only thing that stops an
// instance carrying a pin the template has moved past: the file used to be `owned`,
// so an upgrade could not replace it, and dropping it from the index is what hands
// the decision back to the llz that ships the constraint. A rule that quietly
// matched nothing would strand exactly the instances the change was written for,
// and they would look fine — a stale lock is invisible until `tofu init` refuses it.
//
// It asserts the rules against REPRESENTATIVE paths rather than a live instance,
// because there is no instance here to read. That is the weaker half of the
// property and is deliberate: what can go wrong in this file is the glob, and a
// concrete path is enough to catch it.
func TestTheShippedRemovalRulesMatchRealInstancePaths(t *testing.T) {
	rules, err := readTemplateRemovals(shippedRemovals)
	if err != nil {
		t.Fatalf("read the shipped %s: %v", shippedRemovals, err)
	}
	if len(rules) == 0 {
		t.Fatal("the shipped .template-removals declares no rules — either it moved, or this " +
			"test is asserting nothing. It cannot tell the difference, so it fails.")
	}

	// One path per rule the template ships today, spelled as `git ls-files` emits it.
	// A rule added later with no entry here fails the coverage arm below, which is
	// the point: writing the path down is how the author says what it should hit.
	wantMatch := map[string]string{
		"terraform-iac-bootstrap/*/[a-z]*.tfvars":       "terraform-iac-bootstrap/cluster/prod.tfvars",
		"terraform-iac-bootstrap/*/.terraform.lock.hcl": "terraform-iac-bootstrap/cluster/.terraform.lock.hcl",
		".template-version":                             ".template-version",
		".template-workflows.lock":                      ".template-workflows.lock",
		// The retired apl-core values base and the per-env copies `llz render`
		// used to produce from it. TWO rules, and the samples show why one would
		// not do: filepath.Match's `*` does not span '/', so the base pattern
		// cannot reach apl-values/<env>/values.yaml.
		"apl-values/values.yaml":   "apl-values/values.yaml",
		"apl-values/*/values.yaml": "apl-values/prod/values.yaml",
	}
	for _, r := range rules {
		sample, ok := wantMatch[r.glob]
		if !ok {
			t.Errorf("the shipped rule %q (%s) has no representative path here — add one, so this "+
				"test says what the rule is meant to hit instead of trusting that it hits something",
				r.glob, r.mode)
			continue
		}
		matched, err := filepath.Match(r.glob, sample)
		if err != nil {
			t.Errorf("rule %q is not a valid filepath.Match pattern: %v", r.glob, err)
			continue
		}
		if !matched {
			t.Errorf("rule %q matches nothing at %q — `llz upgrade` would apply it, find no tracked "+
				"file and say nothing, leaving every instance carrying what it was meant to remove",
				r.glob, sample)
		}
	}

	// The other direction: a lock rule that ALSO swallowed the tracked
	// terraform.tfvars.example beside it would remove a file the template ships.
	for _, keep := range []string{
		"terraform-iac-bootstrap/cluster/terraform.tfvars.example",
		"terraform-iac-bootstrap/AGENTS.md",
		"terraform-iac-bootstrap/.gitignore",
		"terraform-iac-bootstrap/cluster/extra-acl-cidrs.txt",
	} {
		for _, r := range rules {
			if matched, _ := filepath.Match(r.glob, keep); matched {
				t.Errorf("rule %q (%s) also matches %q, which the instance keeps", r.glob, r.mode, keep)
			}
		}
	}
}
