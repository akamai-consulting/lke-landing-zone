package providerlock

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// instance_test.go — the gate on the instance-side half.
//
// THE REGRESSION. gsap-apl merged its v0.0.45 upgrade PR with repo-readiness
// green and `terraform-iac-bootstrap/cluster/.terraform.lock.hcl` still pinning
// linode/linode 3.12.0 against a constraint the same release had raised to
// `~> 4.3`. The mismatch first surfaced in a production apply. The lock fixture
// below is that file's real stanza.

const staleInstanceLock = `# This file is maintained automatically by "tofu init".
provider "registry.opentofu.org/linode/linode" {
  version     = "3.12.0"
  constraints = "~> 3.11"
  hashes = ["h1:abc="]
}
`

const freshInstanceLock = `provider "registry.opentofu.org/linode/linode" {
  version     = "4.3.0"
  constraints = "~> 4.3"
  hashes = ["h1:def="]
}
`

// clusterVersions stands in for the embedded cluster root. PINNED HERE rather
// than read from tfroots.RootVersions(): the real one moves every time a
// dependabot PR lands, and a fixture that tracks the code under test asserts
// nothing.
var clusterVersions = map[string]string{"cluster": `
terraform {
  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 4.3"
    }
  }
}
`}

// instanceTree writes an instance-shaped repo: locks at the ROOT's
// terraform-iac-bootstrap/, which is where an adopter's live.
func instanceTree(t *testing.T, locks map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for root, body := range locks {
		d := filepath.Join(dir, "terraform-iac-bootstrap", root)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, lockFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(locks) == 0 {
		if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestScanInstanceCatchesTheStalePin(t *testing.T) {
	dir := instanceTree(t, map[string]string{"cluster": staleInstanceLock})
	results, err := ScanInstance(testRepo(dir), clusterVersions)
	if err != nil {
		t.Fatalf("ScanInstance: %v", err)
	}
	if len(results) != 1 || len(results[0].Violations) != 1 {
		t.Fatalf("results = %+v, want one root with one violation", results)
	}
	v := results[0].Violations[0]
	if v.Provider != "linode/linode" || v.Locked != "3.12.0" || v.Spec != "~> 4.3" {
		t.Errorf("violation = %+v, want linode/linode 3.12.0 vs ~> 4.3", v)
	}
}

func TestScanInstancePassesASatisfiedPin(t *testing.T) {
	dir := instanceTree(t, map[string]string{"cluster": freshInstanceLock})
	results, err := ScanInstance(testRepo(dir), clusterVersions)
	if err != nil {
		t.Fatalf("ScanInstance: %v", err)
	}
	if len(results) != 1 || len(results[0].Violations) != 0 {
		t.Fatalf("results = %+v, want one clean root", results)
	}
	if results[0].Compared == 0 {
		t.Error("a clean root must still record what it compared — a pass over zero comparisons is not a pass")
	}
}

// NO LOCK COMMITTED is a pass: with nothing pinned, `tofu init` resolves fresh
// inside the constraint and cannot be stale.
func TestRunInstancePassesWithNoLockCommitted(t *testing.T) {
	dir := instanceTree(t, nil)
	var out, errOut bytes.Buffer
	if err := RunInstance(dir, &out, &errOut); err != nil {
		t.Fatalf("no committed lock must pass: %v", err)
	}
	if !strings.Contains(out.String(), "commits no .terraform.lock.hcl") {
		t.Errorf("the pass must say what it examined; got %q", out.String())
	}
}

// THE TREE MOVING OUT FROM UNDER THE GATE is the vacuity that IS dangerous, and
// it must not read as "no locks committed".
func TestScanInstanceFailsWhenTheTerraformTreeIsAbsent(t *testing.T) {
	if _, err := ScanInstance(testRepo(t.TempDir()), clusterVersions); err == nil {
		t.Fatal("a repo with no terraform-iac-bootstrap/ must be an error, not a silent pass")
	}
}

// A committed lock this gate cannot read must not pass as "nothing to compare".
func TestScanInstanceFailsOnAnUnreadableLock(t *testing.T) {
	dir := instanceTree(t, map[string]string{"cluster": "# nothing parseable here\n"})
	if _, err := ScanInstance(testRepo(dir), clusterVersions); err == nil {
		t.Fatal("a lock recording no provider must be an error, not an empty comparison")
	}
}

// A root whose constraints cannot be read must not pass either — otherwise a
// regression in the embedded roots empties the expected set and the gate goes
// green on the very bug it exists to catch.
func TestScanInstanceFailsWhenTheShippedRootConstrainsNothing(t *testing.T) {
	dir := instanceTree(t, map[string]string{"cluster": staleInstanceLock})
	if _, err := ScanInstance(testRepo(dir), map[string]string{"cluster": "terraform {}\n"}); err == nil {
		t.Fatal("a shipped root declaring no constraint must be an error")
	}
}

// The report has to name the remedy — the fix is a command an operator runs in
// their own repo, and it is not one they would guess.
//
// THE ROOT IS NAMED, NOT PLACEHELD. This asserted `terraform-iac-bootstrap/<root>`,
// which is what the report literally printed: the operator was handed a path with
// a metavariable still in it and had to map it back onto the violation list above.
// Naming the directory each violation is actually in is the difference between a
// paste and a translation, and a `<root>` reaching the terminal again is the
// regression this arm catches.
//
// Whether the command it names WORKS is not decidable from a substring, and is
// held by TestTheRemedyIsTheCommandThatWorks, which runs it.
func TestRunInstanceReportNamesTheRemedy(t *testing.T) {
	dir := instanceTree(t, map[string]string{"cluster": staleInstanceLock})
	var out, errOut bytes.Buffer
	err := RunInstance(dir, &out, &errOut)
	if err == nil {
		t.Fatal("a stale pin must fail")
	}
	for _, want := range []string{RegenerateCmd, "terraform-iac-bootstrap/cluster", "3.12.0"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("report must contain %q; got:\n%s", want, errOut.String())
		}
	}
	if strings.Contains(errOut.String(), "<root>") {
		t.Errorf("the remedy still carries a `<root>` placeholder the operator has to resolve:\n%s", errOut.String())
	}
}
