package cli

import (
	"strings"
	"testing"
)

func TestBuildFailureSummaryFillsTheDeployment(t *testing.T) {
	out := buildFailureSummary("cluster", "lab")
	// Every command must be runnable as printed. A summary that tells a panicking
	// operator to run `llz build <env> --yes` has made them do the substitution at
	// the worst moment.
	for _, want := range []string{
		"llz build lab --yes",
		"llz up lab --yes",
		"-f region=lab",
		"confirm_destroy=destroy:lab:cluster",
		"llz reap --region",
		"docs/runbooks/first-build-failed.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<env>") {
		t.Errorf("summary left an unsubstituted placeholder:\n%s", out)
	}
}

func TestBuildFailureSummaryDegradesWithoutAStage(t *testing.T) {
	// The verb must still orient when called with nothing: it runs in an
	// already-failing job, so refusing to render is the one thing it must not do.
	out := buildFailureSummary("", "")
	if !strings.Contains(out, "idempotent") {
		t.Errorf("unknown stage lost the re-run guidance:\n%s", out)
	}
	// With no deployment the placeholder is honest — better than naming the wrong
	// one — but it must still be visibly a placeholder.
	if !strings.Contains(out, "<env>") {
		t.Errorf("expected a visible placeholder with no --region:\n%s", out)
	}
}

func TestEveryWiredStageHasItsOwnText(t *testing.T) {
	// The workflows call this with exactly these four stages. A stage that falls
	// through to the generic text has silently lost the one thing this verb adds:
	// what exists on the account at THAT point.
	for _, stage := range []string{"vpc", "cluster", "object-storage", "bootstrap"} {
		if _, ok := stageRecovery[stage]; !ok {
			t.Errorf("stage %q is wired in a workflow but has no recovery text", stage)
		}
	}
}

func TestVPCStageDoesNotMisnameAPreflightFailure(t *testing.T) {
	// The first job hosts the pipeline-wide preflights AND the shared-VPC apply,
	// and the preflights are what usually fails. Calling that "the shared VPC apply
	// failed" sends an operator whose real problem is a PAT scope into Terraform.
	out := stageRecovery["vpc"]
	if strings.Contains(out, "shared VPC apply failed") {
		t.Errorf("vpc text misnames a preflight failure as a Terraform failure:\n%s", out)
	}
	if !strings.Contains(out, "preflight") {
		t.Errorf("vpc text should point at the preflights, which are what fails here:\n%s", out)
	}
}
