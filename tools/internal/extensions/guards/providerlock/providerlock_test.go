package providerlock

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// ── The constraint evaluator ─────────────────────────────────────────────────
//
// This is the whole verdict, so it gets the most cases. `~>` is the one that
// matters in practice — every constraint this repo ships today is pessimistic —
// and it is also the one with a rule people misremember: `~> 3.11` allows 3.99,
// `~> 3.11.0` does not.

func TestSatisfies(t *testing.T) {
	for _, tc := range []struct {
		have, spec string
		want       bool
	}{
		// The shipped shape, and the bump that strands an adopter.
		{"3.12.0", "~> 3.11", true},
		{"3.13.0", "~> 3.11", true},
		{"3.12.0", "~> 4.0", false},
		{"4.0.1", "~> 4.0", true},
		{"3.10.0", "~> 3.11", false}, // below the floor, not just outside the ceiling
		// Two components vary the minor; three hold it.
		{"3.99.0", "~> 3.11", true},
		{"3.12.0", "~> 3.11.2", false},
		{"3.11.9", "~> 3.11.2", true},
		{"3.11.1", "~> 3.11.2", false},
		// Zero-padding: 3.12 and 3.12.0 are the same version.
		{"3.12", "~> 3.12.0", true},
		// The other operators.
		{"3.12.0", ">= 3.0", true},
		{"3.12.0", "< 3.0", false},
		{"3.12.0", "> 3.12.0", false},
		{"3.12.0", ">= 3.12.0", true},
		{"3.12.0", "<= 3.12.0", true},
		{"3.12.0", "= 3.12.0", true},
		{"3.12.0", "3.12.0", true}, // bare version means =
		{"3.12.0", "!= 3.12.0", false},
		// Comma-separated clauses must ALL hold — this is how a root's constraint
		// and a module's are combined.
		{"3.12.0", ">= 3.0, < 4.0", true},
		{"4.1.0", ">= 3.0, < 4.0", false},
		{"3.12.0", "~> 3.11, ~> 4.0", false},
		// Lock versions can carry prerelease metadata; ordering by it is outside
		// what this gate claims, but it must not crash or silently pass.
		{"3.12.0-beta1", "~> 3.11", true},
	} {
		t.Run(tc.have+" vs "+tc.spec, func(t *testing.T) {
			got, err := Satisfies(tc.have, tc.spec)
			if err != nil {
				t.Fatalf("Satisfies(%q, %q): %v", tc.have, tc.spec, err)
			}
			if got != tc.want {
				t.Errorf("Satisfies(%q, %q) = %t, want %t", tc.have, tc.spec, got, tc.want)
			}
		})
	}
}

// AN UNPARSEABLE INPUT IS AN ERROR, NEVER A PASS. If a constraint spelling this
// gate does not understand were treated as satisfied, the gate would go quiet on
// exactly the novel syntax someone just introduced.
func TestSatisfiesRefusesWhatItCannotJudge(t *testing.T) {
	for _, tc := range []struct{ have, spec string }{
		{"3.12.0", "~> latest"},
		{"3.12.0", ">= v3"},
		{"not-a-version", "~> 3.11"},
		{"3.12.0", "^3.11"}, // npm syntax; Terraform has no such operator
	} {
		if ok, err := Satisfies(tc.have, tc.spec); err == nil {
			t.Errorf("Satisfies(%q, %q) = %t with no error; it must refuse what it cannot judge",
				tc.have, tc.spec, ok)
		}
	}
}

// ── Parsing ──────────────────────────────────────────────────────────────────

const clusterVersionsTF = `terraform {
  required_version = ">= 1.5.0"

  required_providers {
    linode = {
      source  = "linode/linode"
      version = "~> 3.11"
    }
    time = {
      source  = "hashicorp/time"
      version = "~> 0.12"
    }
  }
}
`

func TestParseConstraints(t *testing.T) {
	got := ParseConstraints(clusterVersionsTF, "versions.tf")
	if len(got) != 2 {
		t.Fatalf("parsed %d constraint(s), want 2: %+v", len(got), got)
	}
	byProvider := map[string]string{}
	for _, c := range got {
		byProvider[c.Provider] = c.Spec
		if c.From != "versions.tf" {
			t.Errorf("constraint %q lost its source file: %q", c.Provider, c.From)
		}
	}
	if byProvider["linode/linode"] != "~> 3.11" {
		t.Errorf("linode/linode = %q, want ~> 3.11", byProvider["linode/linode"])
	}
	if byProvider["hashicorp/time"] != "~> 0.12" {
		t.Errorf("hashicorp/time = %q, want ~> 0.12", byProvider["hashicorp/time"])
	}
}

// `required_version` sits in the same `terraform {` block and is NOT a provider.
// An earlier cut of the block regex swept it up as one.
func TestParseConstraintsIgnoresRequiredVersion(t *testing.T) {
	for _, c := range ParseConstraints(clusterVersionsTF, "versions.tf") {
		if strings.Contains(c.Provider, "required_version") || c.Spec == ">= 1.5.0" {
			t.Errorf("required_version parsed as a provider: %+v", c)
		}
	}
}

// A .tf file with no required_providers block yields nothing — and must not
// scavenge `x = { ... }` assignments from elsewhere in the file.
func TestParseConstraintsIgnoresNonProviderBlocks(t *testing.T) {
	body := `locals {
  tags = {
    source  = "not/a-provider"
    version = "9.9.9"
  }
}
`
	if got := ParseConstraints(body, "main.tf"); len(got) != 0 {
		t.Errorf("parsed %+v from a file with no required_providers block", got)
	}
}

const clusterLock = `# This file is maintained automatically by "tofu init".
provider "registry.opentofu.org/hashicorp/external" {
  version     = "2.4.0"
  constraints = "~> 2.0"
  hashes = [
    "h1:aaa=",
  ]
}

provider "registry.opentofu.org/linode/linode" {
  version     = "3.12.0"
  constraints = "~> 3.11"
  hashes = [
    "h1:bbb=",
  ]
}
`

func TestParseLock(t *testing.T) {
	got := ParseLock(clusterLock)
	if len(got) != 2 {
		t.Fatalf("parsed %d provider(s), want 2: %+v", len(got), got)
	}
	// THE NORMALISATION IS THE LOAD-BEARING PART. The lock says
	// registry.opentofu.org/linode/linode and required_providers says
	// linode/linode; comparing the raw strings finds no overlap at all, and the
	// gate then reports a clean tree having matched nothing.
	want := map[string]string{"hashicorp/external": "2.4.0", "linode/linode": "3.12.0"}
	for _, l := range got {
		if want[l.Provider] != l.Version {
			t.Errorf("got %s@%s, want %v", l.Provider, l.Version, want)
		}
	}
}

// The `version` inside a lock stanza must not be confused with the `constraints`
// line beside it — they are different fields with the same shape.
func TestParseLockReadsVersionNotConstraints(t *testing.T) {
	for _, l := range ParseLock(clusterLock) {
		if strings.ContainsAny(l.Version, "~<>=") {
			t.Errorf("%s: read a constraint (%q) where a version belongs", l.Provider, l.Version)
		}
	}
}

func TestModulesOf(t *testing.T) {
	mainTF := `module "cluster" {
  source = "git::ssh://git@github.com/<@ upstream_org @>/lke-landing-zone.git//terraform-modules/llz-cluster?ref=<@ llz_version @>"
  # source = "../../terraform-modules/llz-cluster"
}
`
	got := ModulesOf(mainTF)
	if len(got) != 1 || got[0] != "llz-cluster" {
		t.Errorf("ModulesOf = %v, want [llz-cluster]", got)
	}
}

// ── The verdict ──────────────────────────────────────────────────────────────

// THE BEHAVIOR THIS GATE EXISTS FOR: a lock that cannot satisfy the shipped
// constraint is a violation, and the violation names both numbers and the file
// that declared the constraint.
func TestCheckRootFlagsAPinBelowTheConstraint(t *testing.T) {
	res := CheckRoot("cluster",
		[]Constraint{{Provider: "linode/linode", Spec: "~> 4.0", From: "roots/cluster/versions.tf"}},
		[]Locked{{Provider: "linode/linode", Version: "3.12.0"}})

	if len(res.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly 1", res.Violations)
	}
	v := res.Violations[0]
	if v.Locked != "3.12.0" || v.Spec != "~> 4.0" || v.DeclaredIn != "roots/cluster/versions.tf" {
		t.Errorf("violation does not carry what the reader needs to act: %+v", v)
	}
	if res.Compared != 1 {
		t.Errorf("Compared = %d, want 1", res.Compared)
	}
}

func TestCheckRootPassesASatisfyingPin(t *testing.T) {
	res := CheckRoot("cluster",
		[]Constraint{{Provider: "linode/linode", Spec: "~> 3.11", From: "roots/cluster/versions.tf"}},
		[]Locked{{Provider: "linode/linode", Version: "3.12.0"}})
	if len(res.Violations) != 0 {
		t.Errorf("violations = %+v, want none", res.Violations)
	}
	if res.Compared != 1 {
		t.Errorf("Compared = %d, want 1 — a pass must be evidence that something was checked", res.Compared)
	}
}

// A root and a module constraining the same provider are COMBINED, not
// last-one-wins: `tofu init` requires both to hold, and dropping either would let
// a module tighten a constraint the gate then ignores.
func TestCheckRootCombinesRootAndModuleConstraints(t *testing.T) {
	res := CheckRoot("cluster",
		[]Constraint{
			{Provider: "linode/linode", Spec: ">= 3.0", From: "roots/cluster/versions.tf"},
			{Provider: "linode/linode", Spec: "~> 4.0", From: "terraform-modules/llz-cluster/versions.tf"},
		},
		[]Locked{{Provider: "linode/linode", Version: "3.12.0"}})

	if len(res.Violations) != 1 {
		t.Fatalf("the module's tighter constraint was dropped: %+v", res)
	}
	if !strings.Contains(res.Violations[0].DeclaredIn, "llz-cluster") {
		t.Errorf("violation must name both declaring files, got %q", res.Violations[0].DeclaredIn)
	}
}

// The two NON-fatal states. Both are real in the tree today, and neither breaks
// an adopter — failing on them would make the gate cry wolf on a working state,
// which is how a gate gets deleted.
func TestCheckRootReportsButDoesNotFailOnAbsentAndStaleEntries(t *testing.T) {
	res := CheckRoot("cluster",
		[]Constraint{{Provider: "hashicorp/time", Spec: "~> 0.12", From: "terraform-modules/llz-cluster/versions.tf"}},
		[]Locked{{Provider: "hashicorp/local", Version: "2.8.0"}})

	if len(res.Violations) != 0 {
		t.Errorf("violations = %+v; neither an absent pin nor a stale one breaks tofu init", res.Violations)
	}
	if len(res.Notes) != 2 {
		t.Fatalf("notes = %+v, want one for the stale pin and one for the absent one", res.Notes)
	}
	if res.Compared != 0 {
		t.Errorf("Compared = %d, want 0 — nothing lined up, and the caller must be able to see that", res.Compared)
	}
}

// ── Fail-closed arms ─────────────────────────────────────────────────────────
//
// Every one of these is a state in which the gate could report success having
// examined nothing. docs/e2e-gates.md: that is indistinguishable from the outage.

func TestRunFailsWhenNothingLinedUp(t *testing.T) {
	// A tree whose lock and constraints share no provider — what a normalisation
	// bug looks like from the outside.
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf": clusterVersionsTF,
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": `
provider "registry.opentofu.org/someone/else" {
  version = "1.0.0"
}
`,
	})
	err := Run(repo, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "compared NOTHING") {
		t.Fatalf("a run that compared nothing must fail, got %v", err)
	}
}

func TestScanFailsWhenTheRootsAreGone(t *testing.T) {
	if _, err := Scan(testRepo(t.TempDir())); err == nil {
		t.Fatal("a repo with no TF roots must be an error, not a clean scan")
	}
}

func TestScanFailsOnALockThatRecordsNoProvider(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf":               clusterVersionsTF,
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": "# nothing here\n",
	})
	_, err := Scan(testRepo(repo))
	if err == nil || !strings.Contains(err.Error(), "records no provider") {
		t.Fatalf("an empty lock must be an error (the stanza format may have moved), got %v", err)
	}
}

func TestScanFailsOnARootThatConstrainsNothing(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf":               "terraform {\n  required_version = \">= 1.5.0\"\n}\n",
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": clusterLock,
	})
	_, err := Scan(testRepo(repo))
	if err == nil || !strings.Contains(err.Error(), "declares no provider constraint") {
		t.Fatalf("a root that pins providers but constrains none cannot be judged, got %v", err)
	}
}

// A root that ships NO lockfile is skipped, not failed — vpc and databases are
// exactly that, and their providers resolve fresh on every init.
func TestScanSkipsRootsWithoutALock(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf":               clusterVersionsTF,
		"tools/internal/shared/tfroots/roots/vpc/versions.tf":                   clusterVersionsTF,
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": clusterLock,
	})
	got, err := Scan(testRepo(repo))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 || got[0].Root != "cluster" {
		t.Errorf("scanned %+v, want only the root that ships a lock", got)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ── The report ───────────────────────────────────────────────────────────────

// A failure has to teach the reader something they do not already know: that a
// green e2e proves nothing here, and that the fix has two halves. This is the
// whole value of the gate — a bare "constraint not satisfied" sends a maintainer
// to regenerate the lock and ship the bump anyway, stranding every instance in
// the field exactly as before.
func TestRunExplainsTheAsymmetryAndBothHalvesOfTheFix(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf": strings.Replace(
			clusterVersionsTF, `version = "~> 3.11"`, `version = "~> 4.0"`, 1),
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": clusterLock,
	})
	var out, errOut bytes.Buffer
	err := Run(repo, &out, &errOut)
	if err == nil {
		t.Fatal("a lock that cannot satisfy the shipped constraint must fail the gate")
	}
	report := errOut.String()
	for _, want := range []string{
		"3.12.0",             // what is pinned
		"~> 4.0",             // what is required
		"tofu init -upgrade", // the command that regenerates the lock
		".template-removals", // the half that decides what EXISTING instances do
		"release-e2e",        // why a green pipeline is not evidence
	} {
		if !strings.Contains(report, want) {
			t.Errorf("failure report never mentions %q — the reader cannot act on it:\n%s", want, report)
		}
	}
	// A GitHub annotation only parses at the start of a line.
	if !strings.Contains(report, "\n::error file=") && !strings.HasPrefix(report, "::error file=") {
		t.Errorf("no line-initial ::error annotation, so CI will not surface this on the file:\n%s", report)
	}
}

// The clean path must say how much it compared. "OK" over two roots and over
// zero are the same word and very different claims.
func TestRunReportsWhatItCompared(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf":               clusterVersionsTF,
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": clusterLock,
	})
	var out, errOut bytes.Buffer
	if err := Run(repo, &out, &errOut); err != nil {
		t.Fatalf("Run: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "1 provider pin(s)") {
		t.Errorf("the pass does not say how much it checked:\n%s", out.String())
	}
}

// testRepo builds the SAME fenced handle Run builds, so the tests exercise the
// real read path rather than a filesystem the guard never uses in production.
func testRepo(root string) capability.Repo { return capability.RepoForGate(Extension(), root) }
