package configreadiness

// require_repo_config_test.go — the decision logic of `llz ci require-repo-config`.
//
// The verb exists because v0.0.42 made TF_STATE_ENCRYPTION_PASSPHRASE required
// and an adopter first heard about it from a failed `terraform init`. These tests
// hold the two properties that make it worth running on every pull request: it
// must catch a REQUIRED repo-level value that is absent, and it must not report
// anything else — a gate that fails on an env-scoped secret a delivered job
// structurally cannot see would fail every correctly configured instance, and
// would be switched off within a week.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
)

func TestRepoLevelRequirementsAreOnlyWhatAJobCanSee(t *testing.T) {
	reqs := RepoLevelRequirements()
	if len(reqs) == 0 {
		t.Fatal("no repo-level requirements resolved — the filter or the table moved, and every " +
			"assertion below would pass having checked nothing")
	}
	for _, r := range reqs {
		if r.EnvScope {
			t.Errorf("%s is infra-<env> scoped — a job with no `environment:` cannot see it, so "+
				"demanding it would fail a correctly configured instance", r.Name)
		}
		if !r.Required {
			t.Errorf("%s is optional — failing a PR on an optional value makes the gate noise", r.Name)
		}
		if r.Template {
			t.Errorf("%s lives on the TEMPLATE repo (the e2e harness), not on an instance — "+
				"no adopter will ever have it", r.Name)
		}
	}

	// The incident itself, pinned. TF_STATE_ENCRYPTION_PASSPHRASE is the one
	// repo-level required SECRET, and the whole reason this verb exists; a
	// refactor of the requirement table that dropped or re-scoped it would leave
	// the gate running and blind.
	names := RepoLevelRequirementNames()
	if !strings.Contains(strings.Join(names, ","), "TF_STATE_ENCRYPTION_PASSPHRASE") {
		t.Errorf("TF_STATE_ENCRYPTION_PASSPHRASE is not in the repo-level required set (%v) — "+
			"the value whose absence this verb was written to catch", names)
	}
}

func TestMissingRepoConfigTreatsBlankAsAbsent(t *testing.T) {
	reqs := []envreq.Requirement{
		{Name: "SET", Secret: true, Required: true},
		{Name: "EMPTY", Secret: true, Required: true},
		{Name: "WHITESPACE", Secret: true, Required: true},
	}
	// An unset GitHub secret interpolates to the EMPTY STRING, not to an unset
	// variable — `${{ secrets.NOPE }}` yields `KEY=`. Treating only "unset" as
	// missing would pass every instance that has never had the secret, which is
	// exactly the population this gate is for.
	env := map[string]string{"SET": "v", "EMPTY": "", "WHITESPACE": "  \n"}
	got := MissingRepoConfig(reqs, func(k string) string { return env[k] })
	if len(got) != 2 || got[0].Name != "EMPTY" || got[1].Name != "WHITESPACE" {
		t.Errorf("missing = %v, want EMPTY and WHITESPACE", namesOf(got))
	}
}

func TestReportRepoConfigPassesWhenEverythingIsSet(t *testing.T) {
	reqs := []envreq.Requirement{{Name: "A", Required: true}, {Name: "B", Required: true}}
	var out, errOut bytes.Buffer
	if err := ReportRepoConfig(reqs, func(string) string { return "v" }, &out, &errOut); err != nil {
		t.Fatalf("a fully configured instance must pass, got: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("a passing run wrote to stderr: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "A: present.") {
		t.Errorf("a passing run should still say WHAT it checked, got: %q", out.String())
	}
}

func TestReportRepoConfigAnnotatesEachMissingValue(t *testing.T) {
	reqs := []envreq.Requirement{
		{Name: "TF_STATE_ENCRYPTION_PASSPHRASE", Secret: true, Required: true, How: "generated + escrowed (ADR 0007)"},
		{Name: "KUBE_IMAGE", Secret: false, Required: true, How: "computed"},
		{Name: "PRESENT", Required: true},
	}
	env := map[string]string{"PRESENT": "v"}
	var out, errOut bytes.Buffer
	err := ReportRepoConfig(reqs, func(k string) string { return env[k] }, &out, &errOut)
	if err == nil {
		t.Fatal("a missing REQUIRED value must fail the job")
	}
	e := errOut.String()
	// GitHub parses an annotation only at the START of a line; one that lands
	// mid-line is invisible in the PR's checks UI, which is the only place an
	// operator will look.
	for _, ln := range strings.Split(strings.TrimSpace(e), "\n") {
		if !strings.HasPrefix(ln, "::error::") {
			t.Errorf("annotation line does not start at column 0 — GitHub will not render it: %q", ln)
		}
	}
	// The KIND matters: an operator fixes a secret and a variable in two different
	// places in the GitHub UI.
	if !strings.Contains(e, "TF_STATE_ENCRYPTION_PASSPHRASE is not set. This repository secret is REQUIRED") {
		t.Errorf("the missing secret is not named as a secret: %q", e)
	}
	if !strings.Contains(e, "KUBE_IMAGE is not set. This repository variable is REQUIRED") {
		t.Errorf("the missing variable is not named as a variable: %q", e)
	}
	if !strings.Contains(e, "ADR 0007") {
		t.Errorf("the requirement's own guidance (How) should reach the operator: %q", e)
	}
	if strings.Contains(e, "PRESENT") {
		t.Errorf("a value that IS set was reported as missing: %q", e)
	}
	// One remediation line, not one per item.
	if n := strings.Count(e, "llz tokens"); n != 1 {
		t.Errorf("the remediation appears %d times, want exactly 1 — an operator who reads N copies reads none", n)
	}
	if !strings.Contains(err.Error(), "TF_STATE_ENCRYPTION_PASSPHRASE") {
		t.Errorf("the returned error should name what is missing, got: %v", err)
	}
}

func TestRequireRepoConfigCmdWiring(t *testing.T) {
	c := RequireRepoConfigCmd()
	if c.Use != "require-repo-config" {
		t.Errorf("verb is spelled %q — the delivered workflow calls `llz ci require-repo-config`", c.Use)
	}
	if c.RunE == nil {
		t.Error("the verb resolves but does nothing")
	}
	if err := c.Args(c, []string{"stray"}); err == nil {
		t.Error("a stray positional must be rejected — `llz ci <verb> <typo>` exiting 0 is how a gate " +
			"gets silently disabled")
	}
}
