package main

// ci_rotation_plan.go implements `llz ci rotation-plan` — the native port of
// llz-secret-rotation.yml's 'Route scope + validate emergency confirmation'
// setup step. One decision table maps (event, cron, scope, confirm, apply
// flags) to the job-gating outputs every downstream rotation job keys on; in
// bash that table was ~140 untestable lines of case/emit. The confirm phrases
// are typed re-confirmation (the same pattern as `llz ci
// assert-destroy-confirm`): an emergency dispatch must spell out exactly what
// it rotates before anything mutates.
//
// Env contract (identical to the step's env: block):
//   EVENT        — github.event_name (schedule | workflow_dispatch)
//   CRON         — github.event.schedule (which schedule fired)
//   SCOPE        — dispatch input: what to rotate
//   REGION       — dispatch input: deployment (lke-admin only)
//   CONFIRM      — dispatch input: typed confirmation phrase
//   REASON       — dispatch input: audit-log reason (required, non-blank)
//   PAT_APPLY / REVOKE_APPLY / TF_STATE_APPLY / TF_STATE_REVOKE_APPLY
//                — dispatch inputs: arm the respective mutation ("true"/"false")
//   ACTOR        — github.actor (summary attribution)
//   DEPLOYMENTS  — JSON array of deployments from `llz env list` (discover job)

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// The two schedules llz-secret-rotation.yml subscribes to.
const (
	cronMonthlyRotate = "0 4 1 * *"  // lke-admin + PAT + TF-state create
	cronDailyRevoke   = "30 3 * * *" // PAT + TF-state revoke-old reapers
)

// rotationInputs is the routing decision's full input surface, read from env.
type rotationInputs struct {
	Event, Cron                      string
	Scope, Region                    string
	Confirm, Reason, Actor           string
	PATApply, RevokeApply            string
	TFStateApply, TFStateRevokeApply string
	Deployments                      string // JSON array
}

// rotationPlan is the routed outcome: which jobs run, with what scope/arming.
// Zero value = everything off (the bash default_off).
type rotationPlan struct {
	RunLKEAdmin         bool
	RunPATCreate        bool
	RunPATPropagateOnly bool
	RunPATRevoke        bool
	RunTFStateCreate    bool
	RunTFStateRevoke    bool
	Regions             string // JSON array
	PATApply            bool
	RevokeApply         bool
	TFStateApply        bool
	TFStateRevokeApply  bool
	Note                string // human routing note (log + summary)
}

func ciRotationPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotation-plan",
		Short: "route a rotation run: schedule/scope → job-gating step outputs",
		Long: "Native port of llz-secret-rotation.yml's 'Route scope + validate emergency\n" +
			"confirmation' step. Maps the trigger (schedule cron, or a dispatch scope +\n" +
			"typed confirmation + reason) onto the run-*/apply step outputs the rotation\n" +
			"jobs gate on, and writes the dispatch audit summary. Fails on a confirm\n" +
			"mismatch, a blank reason, or an unknown scope/cron — nothing downstream\n" +
			"runs unless this routing passed. Env: EVENT, CRON, SCOPE, REGION, CONFIRM,\n" +
			"REASON, *_APPLY, ACTOR, DEPLOYMENTS.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIRotationPlan(rotationInputsFromEnv()) },
	}
}

func rotationInputsFromEnv() rotationInputs {
	return rotationInputs{
		Event:              os.Getenv("EVENT"),
		Cron:               os.Getenv("CRON"),
		Scope:              os.Getenv("SCOPE"),
		Region:             os.Getenv("REGION"),
		Confirm:            os.Getenv("CONFIRM"),
		Reason:             os.Getenv("REASON"),
		Actor:              os.Getenv("ACTOR"),
		PATApply:           os.Getenv("PAT_APPLY"),
		RevokeApply:        os.Getenv("REVOKE_APPLY"),
		TFStateApply:       os.Getenv("TF_STATE_APPLY"),
		TFStateRevokeApply: os.Getenv("TF_STATE_REVOKE_APPLY"),
		Deployments:        os.Getenv("DEPLOYMENTS"),
	}
}

// routeRotation is the pure decision table. Returned errors are routing
// refusals (confirm mismatch / blank reason / unknown scope or cron) — the
// caller turns them into ::error:: annotations.
func routeRotation(in rotationInputs) (rotationPlan, error) {
	var p rotationPlan
	p.Regions = "[]"

	if in.Event == "schedule" {
		switch in.Cron {
		case cronMonthlyRotate:
			p.RunLKEAdmin, p.RunPATCreate, p.RunTFStateCreate = true, true, true
			p.Regions = in.Deployments
			p.PATApply, p.TFStateApply = true, true
			p.Note = fmt.Sprintf("Monthly schedule — lke-admin (deployments: %s) + Linode PAT + TF-state OBJ key.", in.Deployments)
		case cronDailyRevoke:
			p.RunPATRevoke, p.RunTFStateRevoke = true, true
			p.RevokeApply, p.TFStateRevokeApply = true, true
			p.Note = "Daily schedule — Linode PAT + TF-state OBJ key revoke-old reapers."
		default:
			return p, fmt.Errorf("unknown schedule expression: %s", in.Cron)
		}
		return p, nil
	}

	// ── workflow_dispatch ──
	if strings.TrimSpace(in.Reason) == "" {
		return p, fmt.Errorf("a non-empty reason is required for any dispatch (audit log)")
	}

	// requireConfirm gates every dispatch scope on its exact typed phrase.
	requireConfirm := func(expected string) error {
		if in.Confirm != expected {
			return fmt.Errorf("confirmation mismatch. Type exactly '%s'.", expected)
		}
		return nil
	}
	armed := func(s string) bool { return s == "true" }

	switch in.Scope {
	case "lke-admin":
		if err := requireConfirm("rotate:" + in.Region); err != nil {
			return p, err
		}
		p.RunLKEAdmin = true
		p.Regions = fmt.Sprintf("[%q]", in.Region)
	case "linode-pat":
		if err := requireConfirm("rotate:linode-pat"); err != nil {
			return p, err
		}
		p.RunPATCreate = true
		p.PATApply = armed(in.PATApply)
	case "linode-pat-propagate-only":
		// Recovery path: re-runs propagate-linode-pat using the value currently
		// in secrets.LINODE_API_TOKEN (a create that succeeded but failed to
		// propagate to OpenBao). No new PAT is minted, no hash check is
		// performed (no create job to source it from) — operator-asserted.
		if err := requireConfirm("rotate:linode-pat-propagate-only"); err != nil {
			return p, err
		}
		p.RunPATPropagateOnly = true
	case "linode-pat-revoke":
		if err := requireConfirm("rotate:linode-pat-revoke"); err != nil {
			return p, err
		}
		p.RunPATRevoke = true
		p.RevokeApply = armed(in.RevokeApply)
	case "tf-state-key":
		if err := requireConfirm("rotate:tf-state-key"); err != nil {
			return p, err
		}
		p.RunTFStateCreate = true
		p.TFStateApply = armed(in.TFStateApply)
	case "tf-state-key-revoke":
		if err := requireConfirm("rotate:tf-state-key-revoke"); err != nil {
			return p, err
		}
		p.RunTFStateRevoke = true
		p.TFStateRevokeApply = armed(in.TFStateRevokeApply)
	case "all":
		if err := requireConfirm("rotate:all"); err != nil {
			return p, err
		}
		p.RunLKEAdmin, p.RunPATCreate, p.RunPATRevoke = true, true, true
		p.RunTFStateCreate, p.RunTFStateRevoke = true, true
		p.Regions = in.Deployments
		p.PATApply, p.RevokeApply = armed(in.PATApply), armed(in.RevokeApply)
		p.TFStateApply, p.TFStateRevokeApply = armed(in.TFStateApply), armed(in.TFStateRevokeApply)
	default:
		return p, fmt.Errorf("unknown scope: %s", in.Scope)
	}
	return p, nil
}

// outputLines renders the plan as the step-output key=value lines the
// downstream jobs' `needs.setup.outputs.*` expressions consume.
func (p rotationPlan) outputLines() []string {
	b := func(v bool) string { return fmt.Sprintf("%t", v) }
	return []string{
		"run-lke-admin=" + b(p.RunLKEAdmin),
		"run-pat-create=" + b(p.RunPATCreate),
		"run-pat-propagate-only=" + b(p.RunPATPropagateOnly),
		"run-pat-revoke=" + b(p.RunPATRevoke),
		"run-tf-state-create=" + b(p.RunTFStateCreate),
		"run-tf-state-revoke=" + b(p.RunTFStateRevoke),
		"regions=" + p.Regions,
		"pat-apply=" + b(p.PATApply),
		"revoke-apply=" + b(p.RevokeApply),
		"tf-state-apply=" + b(p.TFStateApply),
		"tf-state-revoke-apply=" + b(p.TFStateRevokeApply),
	}
}

func runCIRotationPlan(in rotationInputs) error {
	plan, err := routeRotation(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::%s\n", capitalizeFirst(err.Error()))
		return err
	}
	if plan.Note != "" {
		fmt.Println(plan.Note)
	}
	if err := appendGHAFile("GITHUB_OUTPUT", plan.outputLines()...); err != nil {
		return err
	}
	if in.Event == "schedule" {
		return nil
	}
	return appendGHAFile("GITHUB_STEP_SUMMARY",
		"## Emergency rotation requested",
		"",
		fmt.Sprintf("- Scope: `%s`", in.Scope),
		fmt.Sprintf("- Region: `%s` (lke-admin only)", in.Region),
		fmt.Sprintf("- pat-apply: `%s` / revoke-apply: `%s`", in.PATApply, in.RevokeApply),
		fmt.Sprintf("- tf-state-apply: `%s` / tf-state-revoke-apply: `%s`", in.TFStateApply, in.TFStateRevokeApply),
		"- Reason: "+in.Reason,
		"- Requested by: @"+in.Actor)
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
