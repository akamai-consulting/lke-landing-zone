package upstreamupdates

// applyafter.go — an upgrade that merges has changed nothing on any cluster, and
// the pull request never said so.
//
// ── WHY THIS IS NOT OBVIOUS FROM THE DIFF ─────────────────────────────────────
//
// An instance commits ZERO Terraform code. terraform-iac-bootstrap/*/*.tf is
// gitignored and the roots are generated at every terraform op by the llz inside
// vars.TF_IMAGE. So an upgrade that changes what Terraform will do to a cluster
// shows NO .tf diff — the change travels in the image, selected by the pin.
//
// On top of that the bot's pull request opens as a DRAFT, deliberately, because
// plan-cluster-pr writes Terraform state and skips drafts. Draft plus generated
// roots means the default reviewable artifact for an automated upgrade contains
// no infrastructure delta at all.
//
// And merging applies nothing: llz-terraform.yml runs no plan and no apply on
// push to main (push-noop-notice) — applies are workflow_dispatch only.
//
// Put together, an instance can sit merged-but-unapplied indefinitely with every
// check green, its clusters running the previous release's Terraform, and nothing
// anywhere saying so. `llz drift` cannot see it either: it compares the repo's
// pin against the template head and never asks a cluster.
//
// ── WHAT THIS DOES, AND WHAT IT DOES NOT ──────────────────────────────────────
//
// It names the deployments that need a dispatch, at the two moments someone is
// looking: in the pull request body, and in `llz upgrade`'s own output for the
// operator who upgrades by hand.
//
// It does NOT know whether any of them has since been applied — that would need
// the applied ref recorded somewhere a check can read, which nothing does today.
// So the wording is "these need an apply", never "these are out of date". A
// reminder that overclaims is one people learn to skip.

import (
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// DeploymentsToApply is the instance's deployments, sorted.
//
// It asks the SPEC through clusterspec.LoadInstance — the same call
// onboard.DefaultDoctorEnv makes — rather than globbing environments/*.yaml.
// Reusing it means "what are this instance's deployments" has one answer; a glob
// here would be a second one, and the two would disagree the first time the spec
// gained a deployment the directory layout did not mirror.
//
// Returns nil for a pre-spec instance, an unreadable spec, or one with no
// deployment yet. All three are states where there is nothing useful to say, and
// none is worth failing an upgrade over.
func DeploymentsToApply(root string) []string {
	lz, err := clusterspec.LoadInstance(root)
	if err != nil || lz == nil {
		return nil
	}
	return lz.EnvNames()
}

// applySection is the pull-request paragraph.
//
// NO BACKTICKED CLI COMMANDS — commands go in fenced blocks. The body used to
// live in a workflow heredoc where TestDeliveredWorkflowCommands resolves every
// CLI invocation in a run: script against the real cobra tree, prose included, so
// a backticked command tokenised with its closing backtick and failed to resolve.
// TestPRBodyCarriesNoBacktickedCLICommand pins the shape in case it moves back.
func applySection(envs []string) string {
	var b strings.Builder
	b.WriteString("\n### Merging this changes nothing on any cluster\n")
	b.WriteString("Terraform runs on workflow_dispatch, not on push to main — so this pull request\n" +
		"moves the pin and the pipeline is what carries it to a cluster. Note also that the\n" +
		"roots are generated from the pin at every terraform op and are gitignored, so an\n" +
		"upgrade that changes what Terraform does shows no .tf diff here.\n")
	if len(envs) == 0 {
		b.WriteString("\nThis instance declares no deployment yet, so there is nothing to apply.\n")
		return b.String()
	}
	b.WriteString("\nAfter merging, apply each deployment:\n\n```\n")
	for _, e := range envs {
		fmt.Fprintf(&b, "llz build %s --yes\n", e)
	}
	b.WriteString("```\n")
	b.WriteString("\nNothing here tracks which of them have been applied since — that is not recorded\n" +
		"anywhere a check can read. This is the list of deployments, not a list of stale ones.\n")
	return b.String()
}
