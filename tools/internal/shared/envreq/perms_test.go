package envreq

// perms_test.go — the PERMS column, at the CONSUMER: what an operator actually
// reads off `llz doctor`.
//
// The measurement existing is not the same as it being visible. A scope verdict
// that is probed and then not rendered is precisely the state this change was
// made to leave behind, so these assert the printed report rather than the map
// that feeds it.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

func capturedReport(t *testing.T, reqs []Requirement, validity map[string]tokenprobe.TokenValidity, caps map[string][]tokenprobe.CapabilityResult) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	ReportReadiness(reqs, map[string]string{}, map[string]string{},
		NewLiveState(nil, map[string]bool{"OPENBAO_SECRETS_WRITE_TOKEN": true}, nil, nil), LiveState{}, validity, caps)
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

var permsReqs = []Requirement{
	{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Secret: true, EnvScope: true, Required: true},
}

// A DENIAL REACHES THE PAGE, in the column AND as an actionable note. Both,
// because the column is where the eye lands and the note is where the fix is.
func TestReportReadiness_RendersADenialAndItsRemedy(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapOK, Detail: "authorized"},
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapDenied, Detail: "authenticates, but is NOT authorized (HTTP 403) — the token is under-scoped, not expired"},
		},
	}
	out := capturedReport(t, permsReqs, nil, caps)

	if !strings.Contains(out, "PERMS") {
		t.Error("the table must carry a PERMS column header")
	}
	if !strings.Contains(out, "DENIED") {
		t.Errorf("a refused grant must show in the column; got:\n%s", out)
	}
	if !strings.Contains(out, "under-scoped") {
		t.Errorf("the note must carry the verdict detail; got:\n%s", out)
	}
	// The remediation for THIS check, not the other one on the same token.
	if !strings.Contains(out, "Secrets: read and write") {
		t.Errorf("the note must print the matching hint — the wrong permission sends the operator to a toggle that changes nothing; got:\n%s", out)
	}
}

// A VALID-BUT-DENIED CREDENTIAL MUST NOT READ AS FINE. This is the exact line
// that shipped for weeks: valid, in date, and unable to do its job.
func TestReportReadiness_ValidDoesNotImplyScoped(t *testing.T) {
	validity := map[string]tokenprobe.TokenValidity{
		"OPENBAO_SECRETS_WRITE_TOKEN": {Name: "OPENBAO_SECRETS_WRITE_TOKEN", Status: tokenprobe.VValid, Detail: "valid, expires in 10d"},
	}
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapDenied, Detail: "under-scoped"}},
	}
	out := capturedReport(t, permsReqs, validity, caps)
	line := credentialLine(t, out, "OPENBAO_SECRETS_WRITE_TOKEN")
	if !strings.Contains(line, "valid") {
		t.Errorf("the validity verdict must survive the new column; got %q", line)
	}
	if !strings.Contains(line, "DENIED") {
		t.Errorf("the row must show both verdicts — a green VALID beside a blank PERMS is the report that shipped the outage; got %q", line)
	}
}

// NOTHING PROBED MUST NOT RENDER AS A PASS. A credential whose value is not
// local gets "unprobed", never a tick: a gate that reports success having
// examined nothing looks exactly like the state it exists to catch.
func TestReportReadiness_UnprobedIsNotATick(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Status: tokenprobe.CapSkipped, Detail: "set on GitHub — not in .llz cache"}},
	}
	line := credentialLine(t, capturedReport(t, permsReqs, nil, caps), "OPENBAO_SECRETS_WRITE_TOKEN")
	if strings.Contains(line, "✓ scoped") {
		t.Errorf("an unprobed credential must not claim to be scoped; got %q", line)
	}
	if !strings.Contains(line, "unprobed") {
		t.Errorf("an unprobed credential must say so; got %q", line)
	}

	// And with no capability map at all (probing off), the same rule holds.
	line = credentialLine(t, capturedReport(t, permsReqs, nil, nil), "OPENBAO_SECRETS_WRITE_TOKEN")
	if strings.Contains(line, "✓ scoped") {
		t.Errorf("scope probing disabled must not render as authorized; got %q", line)
	}
}

// A REQUIREMENT WITH NO SCOPE CHECK LEAVES THE COLUMN BLANK rather than ticking
// it — same vacuity rule, applied to the credential nobody registered a check
// for.
func TestReportReadiness_NoRegisteredCheckRendersBlank(t *testing.T) {
	reqs := []Requirement{{Name: "TF_STATE_BUCKET", Secret: false, Required: true}}
	line := credentialLine(t, capturedReport(t, reqs, nil, nil), "TF_STATE_BUCKET")
	for _, forbidden := range []string{"✓ scoped", "unprobed", "DENIED"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("a requirement with no scope check must render a blank PERMS cell, got %q in %q", forbidden, line)
		}
	}
}

// A VERIFIED GRANT BESIDE AN UNASKED ONE IS NOT A PASS. This is the mixed case
// the column's reduction used to lose: CapSkipped ranked BELOW CapOK, so one
// grant probed clean and one never asked reduced to "✓ scoped" — a tick standing
// for a question half of which was not put. The all-skipped case was already
// gated above; a reduction that only holds when nothing was probed is not the
// rule, it is one arm of it.
func TestReportReadiness_PartiallyProbedIsNotATick(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapSkipped, Detail: "repo/region unknown"},
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapOK, Detail: "authorized"},
		},
	}
	line := credentialLine(t, capturedReport(t, permsReqs, nil, caps), "OPENBAO_SECRETS_WRITE_TOKEN")
	if strings.Contains(line, "✓ scoped") {
		t.Errorf("a credential with an unasked grant must not render as scoped; got %q", line)
	}
	// And it must not read as wholly unprobed either — one grant WAS verified, and
	// a word that hides that sends the operator to re-check work already done.
	if !strings.Contains(line, "partial") {
		t.Errorf("a partially probed credential must say so; got %q", line)
	}
}

// AT THE CONSUMER: the skip detail must reach the PAGE. The model carries one
// CapSkipped row per registered check, each naming its op and pointing at where
// the question can be answered — and the notes loop rendered three statuses,
// none of them CapSkipped, so every word of it was written for nobody. The
// doctor-side test asserted the same strings off the map, which passes happily
// while the operator sees a bare "· unprobed".
func TestReportReadiness_UnaskedChecksAreExplained(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapSkipped, Detail: "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"},
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapSkipped, Detail: "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"},
		},
	}
	out := capturedReport(t, permsReqs, nil, caps)
	if !strings.Contains(out, "validate-tokens") {
		t.Errorf("the skip detail must reach the report — it names where the question CAN be answered; got:\n%s", out)
	}
	if !strings.Contains(out, "NOT verified") {
		t.Errorf("the note must say the scope was not verified; got:\n%s", out)
	}
	// WHICH grants went unasked, not just that some did.
	for _, op := range []string{"environment secrets", "REPO-level"} {
		if !strings.Contains(out, op) {
			t.Errorf("the note must name the unasked check %q; got:\n%s", op, out)
		}
	}
	// One line per credential, not one per check — two identical details must not
	// print twice.
	if n := strings.Count(out, "scope NOT verified"); n != 1 {
		t.Errorf("scope-unverified notes = %d, want 1 per credential", n)
	}
}

// A CREDENTIAL THAT IS SIMPLY ABSENT GETS NO NOTE. STATUS already says
// "✗ missing"; repeating it under every row is noise on a fresh instance, and
// noise is what got the whole arm left out in the first place.
func TestReportReadiness_AbsentCredentialGetsNoScopeNote(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapSkipped, Detail: tokenprobe.SkipNotSet},
		},
	}
	out := capturedReport(t, permsReqs, nil, caps)
	if strings.Contains(out, "scope NOT verified") {
		t.Errorf("an unset credential must not grow a scope note; got:\n%s", out)
	}
}

// TWO CHECKS, TWO REASONS, TWO LINES. A credential can have one grant unasked
// for want of a cached value and another because its component is not deployed;
// printing both ops under whichever reason came last attaches a real explanation
// to a check it is not about.
func TestReportReadiness_DistinctSkipReasonsAreNotMerged(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapSkipped, Detail: "set on GitHub — not in .llz cache"},
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapSkipped, Detail: "not applicable — this deployment does not enable the `harbor` component"},
		},
	}
	out := capturedReport(t, permsReqs, nil, caps)
	if n := strings.Count(out, "scope NOT verified"); n != 2 {
		t.Fatalf("notes = %d, want 2 — one per distinct reason; got:\n%s", n, out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "harbor") && strings.Contains(l, "environment secrets") {
			t.Errorf("a reason must not be attached to the check it is not about: %q", l)
		}
	}
}

// A CORRECT CONFIGURATION MUST NOT CARRY A STANDING WARNING. When a grant's
// consumer is not deployed there is nothing to verify and nothing to fix, and
// the row said otherwise:
//
//	OPENBAO_SECRETS_WRITE_TOKEN  ✓ set  ✓ valid  · partial
//	  · OPENBAO_SECRETS_WRITE_TOKEN: scope NOT verified (…) — not applicable …
//
// A permanent yellow plus a note whose halves contradict each other, on a
// harbor-less instance that is entirely correct and will never change. That is
// how a column stops being read, which costs exactly the attention the column
// was added to buy.
func TestReportReadiness_InapplicableCheckIsNotAWarning(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapNotApplicable, Detail: "this deployment does not enable the `harbor` component"},
		},
	}
	out := capturedReport(t, permsReqs, nil, caps)
	if strings.Contains(out, "NOT verified") {
		t.Errorf("nothing failed to be verified — there was nothing to verify; got:\n%s", out)
	}
	if strings.Contains(out, "partial") {
		t.Errorf("an inapplicable check must not read as a partial probe; got:\n%s", out)
	}
	line := credentialLine(t, out, "OPENBAO_SECRETS_WRITE_TOKEN")
	if !strings.Contains(line, "n/a") {
		t.Errorf("the column should say the question does not apply; got %q", line)
	}
}

// AND IT MUST NOT MASK A REAL VERDICT. One grant inapplicable, the other
// verified, reads as scoped — everything askable was asked.
func TestReportReadiness_InapplicableDoesNotHideAnAnsweredGrant(t *testing.T) {
	caps := map[string][]tokenprobe.CapabilityResult{
		"OPENBAO_SECRETS_WRITE_TOKEN": {
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapOK, Detail: "authorized"},
			{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "read/write REPO-level Actions secrets", Status: tokenprobe.CapNotApplicable, Detail: "harbor not enabled"},
		},
	}
	line := credentialLine(t, capturedReport(t, permsReqs, nil, caps), "OPENBAO_SECRETS_WRITE_TOKEN")
	if !strings.Contains(line, "scoped") {
		t.Errorf("everything askable was asked and answered — the row should read scoped; got %q", line)
	}
	// ...and a denial still dominates an inapplicable sibling.
	caps["OPENBAO_SECRETS_WRITE_TOKEN"][0] = tokenprobe.CapabilityResult{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Op: "write infra-<region> environment secrets", Status: tokenprobe.CapDenied, Detail: "under-scoped"}
	line = credentialLine(t, capturedReport(t, permsReqs, nil, caps), "OPENBAO_SECRETS_WRITE_TOKEN")
	if !strings.Contains(line, "DENIED") {
		t.Errorf("an inapplicable sibling must not soften a real denial; got %q", line)
	}
}

func credentialLine(t *testing.T, report, name string) string {
	t.Helper()
	for _, l := range strings.Split(report, "\n") {
		if strings.HasPrefix(l, name+" ") {
			return l
		}
	}
	t.Fatalf("no row for %q in:\n%s", name, report)
	return ""
}
