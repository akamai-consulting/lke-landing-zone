package credcoverage

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credtargets"
)

// writeWorkflows lays out a minimal instance-template/.github/workflows tree.
func writeWorkflows(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "instance-template", ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The gate itself: a credential a workflow consumes and nothing measures fails.
func TestCredentialCoverageGuardFailsOnUnmeasuredSecret(t *testing.T) {
	root := writeWorkflows(t, map[string]string{
		"a.yml": "env:\n  X: ${{ secrets.SOME_NEW_KEY }}\n",
	})
	err := runCICredentialCoverageGuard(root)
	if err == nil {
		t.Fatal("an unmeasured credential must fail the guard")
	}
	if !strings.Contains(err.Error(), "unmeasured credential") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Coverage is DERIVED from the target lists, not restated — so adding a target is
// the way to satisfy the guard, and the guard cannot vouch for a credential the
// inventory does not actually measure.
//
// Asserted on `unmeasured` alone: a two-secret corpus leaves every exemption
// unused, so checking the error string would conflate "this is measured" with
// "the registry is stale", which is a different rule with a different remedy.
func TestCredentialCoverageGuardAcceptsMeasuredSecrets(t *testing.T) {
	unmeasured, _ := classifyCredentialCoverage(
		[]string{credtargets.GHSecretTargets[0].Name, credtargets.GHPATTargets[0].Name}, io.Discard)
	if len(unmeasured) != 0 {
		t.Errorf("measured credentials must be accounted for, got %v", unmeasured)
	}
}

// The counterfactual this guard exists for. Before it, OPENBAO_SEAL_KEY — the
// at-rest key for every other credential in the platform — was consumed by the
// bootstrap workflow and measured by nothing, and no gate in the repo said so.
// Re-create that state by measuring everything EXCEPT it.
func TestCredentialCoverageGuardWouldHaveCaughtTheSealKey(t *testing.T) {
	orig := credtargets.GHSecretTargets
	t.Cleanup(func() { credtargets.GHSecretTargets = orig })
	var trimmed []credtargets.SecretTarget
	for _, tgt := range orig {
		if tgt.Name != "OPENBAO_SEAL_KEY" {
			trimmed = append(trimmed, tgt)
		}
	}
	credtargets.GHSecretTargets = trimmed

	unmeasured, _ := classifyCredentialCoverage([]string{"OPENBAO_SEAL_KEY"}, io.Discard)
	if len(unmeasured) != 1 || unmeasured[0] != "OPENBAO_SEAL_KEY" {
		t.Fatalf("the pre-fix state must be reported unmeasured, got %v", unmeasured)
	}

	// And with the entry restored — the shipping state — it is accounted for.
	credtargets.GHSecretTargets = orig
	if unmeasured, _ := classifyCredentialCoverage([]string{"OPENBAO_SEAL_KEY"}, io.Discard); len(unmeasured) != 0 {
		t.Errorf("the seal key must now be measured, got %v", unmeasured)
	}
}

// A registry that keeps entries for secrets nothing uses stops being reviewable:
// the next reader cannot tell which lines are load-bearing. Same rule as
// plaintextAllowed's.
func TestCredentialCoverageGuardFailsOnStaleExemption(t *testing.T) {
	root := writeWorkflows(t, map[string]string{
		// Uses one exempt secret; every OTHER exemption is now stale.
		"a.yml": "x: ${{ secrets.GITHUB_TOKEN }}\n",
	})
	err := runCICredentialCoverageGuard(root)
	if err == nil {
		t.Fatal("unused exemptions must fail")
	}
	if !strings.Contains(err.Error(), "stale registry entries") {
		t.Errorf("unexpected error: %v", err)
	}
	_, stale := classifyCredentialCoverage([]string{"GITHUB_TOKEN"}, io.Discard)
	for _, s := range stale {
		if s == "GITHUB_TOKEN" {
			t.Errorf("a USED exemption must not be reported stale")
		}
	}
	if len(stale) != len(credCoverageExempt)-1 {
		t.Errorf("got %d stale entries, want %d", len(stale), len(credCoverageExempt)-1)
	}
}

// `secrets: inherit` is a keyword, not a secret. Matching it would put a
// permanent unregistered finding named "inherit" in front of every reviewer,
// which is how a gate gets disabled.
func TestCredentialCoverageGuardIgnoresSecretsInherit(t *testing.T) {
	dir := filepath.Join(writeWorkflows(t, map[string]string{
		"a.yml": "jobs:\n  call:\n    uses: ./.github/workflows/x.yml\n    secrets: inherit\n",
	}), "instance-template", ".github", "workflows")
	got, n, err := collectWorkflowSecretRefs(ccRepo(dir), ".")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("read %d files, want 1", n)
	}
	if len(got) != 0 {
		t.Errorf("`secrets: inherit` must yield no secret names, got %v", got)
	}
}

// A guard that read no workflows reports the same color.Green as one that read all of
// them — the failure requireCorpus exists to catch.
func TestCredentialCoverageGuardFailsOnEmptyCorpus(t *testing.T) {
	err := runCICredentialCoverageGuard(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "examined 0") {
		t.Fatalf("an empty corpus must fail closed, got %v", err)
	}
}

// The registry's whole value is that a reader can act on it. A kind outside the
// closed vocabulary, or an empty reason, is the "accepted" placeholder this guard
// was written to keep out.
func TestCredCoverageExemptEntriesAreReviewable(t *testing.T) {
	known := map[string]bool{
		credExemptLinodeAccount: true, credExemptEphemeral: true,
		credExemptNotACredential: true, credExemptRotationWindow: true,
	}
	for name, e := range credCoverageExempt {
		if !known[e.kind] {
			t.Errorf("%s: kind %q is outside the closed vocabulary", name, e.kind)
		}
		if len(strings.TrimSpace(e.reason)) < 40 {
			t.Errorf("%s: reason is too short to be a reason: %q", name, e.reason)
		}
	}
}

// The live tree must be color.Green, and it must be color.Green because everything is
// covered rather than because nothing was read.
func TestCredentialCoverageGuardPassesOnThisRepo(t *testing.T) {
	if err := runCICredentialCoverageGuard("../../../../.."); err != nil {
		t.Fatalf("credential-coverage-guard must be color.Green on this repo: %v", err)
	}
}

// A `secrets.FOO` in prose is not a usage. Counting it would keep a retired
// secret's exemption looking live, which defeats the staleness rule this guard
// leans on hardest — registry rot re-entering through the door the check watches.
func TestCredentialCoverageGuardIgnoresWholeLineComments(t *testing.T) {
	dir := filepath.Join(writeWorkflows(t, map[string]string{
		"a.yml": "# historical note: this used to read ${{ secrets.RETIRED_TOKEN }}\n" +
			"env:\n  X: ${{ secrets.GITHUB_TOKEN }}\n",
	}), "instance-template", ".github", "workflows")
	got, _, err := collectWorkflowSecretRefs(ccRepo(dir), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g == "RETIRED_TOKEN" {
			t.Error("a secret named only in a comment must not count as used")
		}
	}
	if len(got) != 1 || got[0] != "GITHUB_TOKEN" {
		t.Errorf("the real usage must survive, got %v", got)
	}
}

// The other error direction, which is NOT symmetric: dropping a real usage means
// an unmeasured credential goes unnoticed, which is the failure this guard
// exists to prevent. So only whole-line comments are stripped — a `#` earlier on
// a line that also carries a live reference must not blind the scan.
func TestCredentialCoverageGuardKeepsUsagesAfterAnInlineHash(t *testing.T) {
	dir := filepath.Join(writeWorkflows(t, map[string]string{
		"a.yml": "steps:\n  - run: echo \"# banner\" && use ${{ secrets.GHCR_READ_TOKEN }}\n",
	}), "instance-template", ".github", "workflows")
	got, _, err := collectWorkflowSecretRefs(ccRepo(dir), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "GHCR_READ_TOKEN" {
		t.Errorf("an inline # must not drop a real usage, got %v", got)
	}
}
