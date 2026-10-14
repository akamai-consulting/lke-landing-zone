package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUsesOrgWithoutARef covers the unpinned `uses:` form. Nothing requires a
// workflow reference to carry @ref — a same-repo `uses: org/repo/.github/...`
// is legal — so the ref-stripping step has to cope with its absence rather than
// assume one is always there.
func TestUsesOrgWithoutARef(t *testing.T) {
	if got, want := usesOrg("akamai-consulting/lke-landing-zone/.github/workflows/x.yml"), "akamai-consulting"; got != want {
		t.Errorf("usesOrg(no @ref) = %q, want %q", got, want)
	}
	if got, want := usesOrg("actions/checkout"), "actions"; got != want {
		t.Errorf("usesOrg(no @ref) = %q, want %q", got, want)
	}
}

// TestUsesOrgRejectsALeadingRef: a value that is nothing but a ref has no owner
// at all. Returning the ref fragment as if it were a GitHub org would make this
// gate compare "@v1" against the instance's org, flag a mismatch, and fail
// doctor on a workflow that never crosses an org boundary.
func TestUsesOrgRejectsALeadingRef(t *testing.T) {
	for _, in := range []string{"@v1/x", "@refs/heads/main", "@v1"} {
		if got := usesOrg(in); got != "" {
			t.Errorf("usesOrg(%q) = %q, want \"\" — a leading @ref names no owner", in, got)
		}
	}
}

// TestCheckCrossOrgReuseFlagsAnInstance drives the doctor section end to end.
// `secrets: inherit` does NOT cross organizations: the called workflow runs with
// every secret EMPTY, which surfaces as an unauthenticated API call deep inside
// a provisioning run rather than as a configuration error. The section is a
// deliberate no-op outside an instance, so the branch that decides "this IS an
// instance" is the difference between a live gate and a permanently silent one.
func TestCheckCrossOrgReuseFlagsAnInstance(t *testing.T) {
	chdirTemp(t)
	if err := os.WriteFile(".copier-answers.yml", []byte("instance_repo: acme/platform-instance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	wf := "jobs:\n" +
		"  call:\n" +
		"    uses: akamai-consulting/lke-landing-zone/.github/workflows/llz-terraform.yml@v0.0.24\n" +
		"    secrets: inherit\n"
	if err := os.WriteFile(filepath.Join(".github", "workflows", "terraform.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() { err = CheckCrossOrgReuse() })
	if err == nil {
		t.Fatalf("a cross-org `secrets: inherit` job in an instance must fail the doctor gate; stdout:\n%s", out)
	}
	if !strings.Contains(err.Error(), "cross-org") {
		t.Errorf("error should name the problem, got: %v", err)
	}
	if !strings.Contains(out, "akamai-consulting") || !strings.Contains(out, "acme") {
		t.Errorf("the report must name both the called org and the instance's own org:\n%s", out)
	}

	// Repointing the call at the instance's own org clears it — proof the gate
	// is comparing orgs and not simply failing on any `uses:`.
	sameOrg := strings.Replace(wf, "akamai-consulting/", "acme/", 1)
	if err := os.WriteFile(filepath.Join(".github", "workflows", "terraform.yml"), []byte(sameOrg), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() { err = CheckCrossOrgReuse() })
	if err != nil {
		t.Errorf("a same-org call must pass, got: %v", err)
	}
}
