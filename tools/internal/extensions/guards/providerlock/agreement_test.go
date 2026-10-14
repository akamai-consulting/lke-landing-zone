package providerlock

import (
	"bytes"
	"strings"
	"testing"
)

// ── The judgement ────────────────────────────────────────────────────────────

func TestCheckAgreementPassesWhenEverySiteAgrees(t *testing.T) {
	got := CheckAgreement([]Constraint{
		{Provider: "linode/linode", Spec: "~> 4.3", From: "roots/cluster/versions.tf"},
		{Provider: "linode/linode", Spec: "~> 4.3", From: "terraform-modules/llz-cluster/versions.tf"},
		{Provider: "hashicorp/time", Spec: "~> 0.12", From: "terraform-modules/llz-cluster/versions.tf"},
	})
	if len(got) != 0 {
		t.Errorf("identical specs are not a conflict, got %v", got)
	}
}

// The PR #504 shape: the bot moved terraform-modules/ and could not reach the
// generated roots.
func TestCheckAgreementFlagsTheHalfMovedBump(t *testing.T) {
	got := CheckAgreement([]Constraint{
		{Provider: "linode/linode", Spec: "~> 3.11", From: "tools/internal/shared/tfroots/roots/cluster/versions.tf"},
		{Provider: "linode/linode", Spec: "~> 3.11", From: "tools/internal/shared/tfroots/roots/vpc/versions.tf"},
		{Provider: "linode/linode", Spec: "~> 4.3", From: "terraform-modules/llz-cluster/versions.tf"},
	})
	if len(got) != 1 {
		t.Fatalf("want exactly one conflict, got %v", got)
	}
	c := got[0]
	if c.Provider != "linode/linode" {
		t.Errorf("conflict names %q", c.Provider)
	}
	// BOTH halves must be named. A report that lists only the sites the bot did
	// not touch reads as "the roots are wrong", when which half to move is the
	// maintainer's call.
	if want := []string{"~> 3.11", "~> 4.3"}; len(c.Specs()) != 2 ||
		c.Specs()[0] != want[0] || c.Specs()[1] != want[1] {
		t.Errorf("specs = %v, want %v", c.Specs(), want)
	}
	if len(c.Sites["~> 3.11"]) != 2 {
		t.Errorf("the two roots at ~> 3.11 must both be listed, got %v", c.Sites["~> 3.11"])
	}
	if len(c.Sites["~> 4.3"]) != 1 {
		t.Errorf("the module at ~> 4.3 must be listed, got %v", c.Sites["~> 4.3"])
	}
}

// A provider declared at exactly one site cannot disagree with anything — vpc's
// linode entry is the only declaration reaching that root.
func TestCheckAgreementIgnoresASingleDeclaration(t *testing.T) {
	got := CheckAgreement([]Constraint{
		{Provider: "hashicorp/time", Spec: "~> 0.12", From: "terraform-modules/llz-cluster/versions.tf"},
	})
	if len(got) != 0 {
		t.Errorf("one declaration is not a conflict, got %v", got)
	}
}

// Two providers can disagree independently; neither may mask the other.
func TestCheckAgreementReportsEveryProviderSortedByName(t *testing.T) {
	got := CheckAgreement([]Constraint{
		{Provider: "linode/linode", Spec: "~> 3.11", From: "a"},
		{Provider: "linode/linode", Spec: "~> 4.3", From: "b"},
		{Provider: "hashicorp/time", Spec: "~> 0.12", From: "c"},
		{Provider: "hashicorp/time", Spec: "~> 0.14", From: "d"},
	})
	if len(got) != 2 {
		t.Fatalf("want both providers reported, got %v", got)
	}
	if got[0].Provider != "hashicorp/time" || got[1].Provider != "linode/linode" {
		t.Errorf("conflicts are not sorted by provider: %v", got)
	}
}

// ── The corpus ───────────────────────────────────────────────────────────────

// The roots that ship NO lockfile are exactly where a conflict is otherwise
// invisible: Scan skips them, so only AllConstraints can see databases and vpc.
func TestAllConstraintsReadsRootsThatShipNoLock(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/vpc/versions.tf": clusterVersionsTF,
		"terraform-modules/llz-cluster/versions.tf":           moduleVersionsTF("~> 3.11"),
	})
	got, err := AllConstraints(testRepo(repo))
	if err != nil {
		t.Fatalf("AllConstraints: %v", err)
	}
	var sawVPC bool
	for _, c := range got {
		if strings.Contains(c.From, "roots/vpc/") {
			sawVPC = true
		}
	}
	if !sawVPC {
		t.Errorf("a lockless root was not read, so a conflict there is invisible: %v", got)
	}
}

// FAIL CLOSED. Each arm below would otherwise report "everything agrees" over a
// corpus it never read — the failure mode regex parsing is most exposed to.
func TestAllConstraintsFailsClosedOnAnEmptyCorpus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "roots tree absent",
			files: map[string]string{"terraform-modules/llz-cluster/versions.tf": moduleVersionsTF("~> 3.11")},
			want:  "read tools/internal/shared/tfroots/roots",
		},
		{
			name:  "modules tree absent",
			files: map[string]string{"tools/internal/shared/tfroots/roots/cluster/versions.tf": clusterVersionsTF},
			want:  "read terraform-modules",
		},
		{
			name: "modules present but constrain nothing",
			files: map[string]string{
				"tools/internal/shared/tfroots/roots/cluster/versions.tf": clusterVersionsTF,
				"terraform-modules/llz-cluster/versions.tf":               "terraform {\n}\n",
			},
			want: "no provider constraint found anywhere under terraform-modules",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			writeTree(t, repo, tc.files)
			_, err := AllConstraints(testRepo(repo))
			if err == nil {
				t.Fatal("an unreadable or empty corpus must fail, not pass over nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A directory with no versions.tf constrains nothing and is not an error — not
// every module or root declares providers.
func TestAllConstraintsSkipsDirectoriesWithoutVersionsTF(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf": clusterVersionsTF,
		"tools/internal/shared/tfroots/roots/vpc/main.tf":         "# no providers here\n",
		"terraform-modules/llz-cluster/versions.tf":               moduleVersionsTF("~> 3.11"),
		"terraform-modules/llz-docs/README.md":                    "not a module\n",
	})
	if _, err := AllConstraints(testRepo(repo)); err != nil {
		t.Fatalf("a directory without versions.tf must be skipped, not fail: %v", err)
	}
}

// ── The report ───────────────────────────────────────────────────────────────

// The agreement check runs BEFORE the lock check and short-circuits it. When the
// specs do not overlap no lock can satisfy them, so reporting the lock as the
// violation — which is what PR #504 actually produced — sends the fix to the
// wrong file.
func TestRunReportsTheConflictRatherThanBlamingTheLock(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		// The exact half-moved bump: roots left behind at 3.11, modules at 4.3.
		"tools/internal/shared/tfroots/roots/cluster/versions.tf":               clusterVersionsTF,
		"terraform-modules/llz-cluster/versions.tf":                             moduleVersionsTF("~> 4.3"),
		"instance-template/terraform-iac-bootstrap/cluster/.terraform.lock.hcl": clusterLock,
	})
	var out, errOut bytes.Buffer
	err := Run(repo, &out, &errOut)
	if err == nil {
		t.Fatal("constraints that cannot both hold must fail the gate")
	}
	report := errOut.String()
	for _, want := range []string{
		"~> 3.11", // the spec left behind
		"~> 4.3",  // the spec the bot moved to
		"tools/internal/shared/tfroots/roots/cluster/versions.tf", // the file to edit
		"terraform-modules/llz-cluster/versions.tf",               // the other half
		"INTERSECTED",   // WHY both have to hold
		"Dependabot",    // who produces this shape unaided
		"the SAME spec", // the remediation
	} {
		if !strings.Contains(report, want) {
			t.Errorf("conflict report never mentions %q — the reader cannot act on it:\n%s", want, report)
		}
	}
	// It must NOT send the reader to regenerate a lock that was never the problem.
	if strings.Contains(report, "tofu init -upgrade") {
		t.Errorf("the conflict report points at the lock, which no regeneration could fix:\n%s", report)
	}
	if !strings.Contains(report, "\n::error file=") && !strings.HasPrefix(report, "::error file=") {
		t.Errorf("no line-initial ::error annotation, so CI will not surface this on the file:\n%s", report)
	}
}

// Every disagreeing site gets its own annotation, so the PR shows the whole fix
// inline rather than one file the reader has to generalise from.
func TestRunAnnotatesEveryDisagreeingSite(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"tools/internal/shared/tfroots/roots/cluster/versions.tf": clusterVersionsTF,
		"tools/internal/shared/tfroots/roots/vpc/versions.tf":     clusterVersionsTF,
		"terraform-modules/llz-cluster/versions.tf":               moduleVersionsTF("~> 4.3"),
	})
	var out, errOut bytes.Buffer
	if err := Run(repo, &out, &errOut); err == nil {
		t.Fatal("want a conflict")
	}
	for _, f := range []string{
		"::error file=tools/internal/shared/tfroots/roots/cluster/versions.tf::",
		"::error file=tools/internal/shared/tfroots/roots/vpc/versions.tf::",
		"::error file=terraform-modules/llz-cluster/versions.tf::",
	} {
		if !strings.Contains(errOut.String(), f) {
			t.Errorf("no annotation for %s:\n%s", f, errOut.String())
		}
	}
}
