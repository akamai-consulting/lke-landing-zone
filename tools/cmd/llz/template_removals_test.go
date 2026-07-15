package main

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
		"platform-apl/manifest/dns/old-webhook.yaml",         // delete
	}
	for _, p := range tracked {
		writeFile(t, filepath.Join(dir, p), "x\n")
	}
	writeFile(t, filepath.Join(dir, ".template-removals"), `untrack  terraform-iac-bootstrap/*/[a-z]*.tfvars
delete   platform-apl/manifest/dns/old-webhook.yaml
`)
	gitInitRepo(t, dir, append(tracked, ".template-removals")...)
	chdir(t, dir)

	if err := applyTemplateRemovals(globalOpts{}); err != nil {
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
	if err := applyTemplateRemovals(globalOpts{}); err != nil {
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
	if err := applyTemplateRemovals(globalOpts{}); err != nil {
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
