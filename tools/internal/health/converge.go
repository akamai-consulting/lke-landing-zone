package health

// converge.go ports the per-iteration decision of converge.sh — the poll loop
// that runs the health check until the cluster converges or the budget is spent.
// The loop's timing/orchestration lives in cmd/llz; this is the exit-code → action
// mapping it branches on.

// ConvergeAction is what the converge loop does after one health-check run.
type ConvergeAction int

const (
	// ConvergeDone: the check converged (exit 0) — stop, success.
	ConvergeDone ConvergeAction = iota
	// ConvergePoll: in-progress (exit 2) — sleep the interval and re-run, until budget.
	ConvergePoll
	// ConvergeRetryHard: hard-failed (exit 1) — sleep the retry delay and re-run
	// ONCE to absorb a transient misclassification before giving up.
	ConvergeRetryHard
	// ConvergeUnreachable: the apiserver could not be reached (exit 3) — an
	// infrastructure transient (apiserver still coming up, a konnectivity blip),
	// NOT a cluster verdict. Re-run against the overall budget without spending a
	// hard strike, so one unreachable poll can't trip the twice-in-a-row abort.
	ConvergeUnreachable
	// ConvergeAbort: an exit code outside the 0/1/2/3 contract — stop, treat as failure.
	ConvergeAbort
)

// PhaseAwareExitCode downgrades a hard-fail (exit 1) to in-progress (exit 2)
// while the cluster is still in the early-bootstrap window — phase1: bootstrap-
// cluster has run but bootstrap-openbao has not completed.
// In phase1 the support plane is provably still installing: apl-core brings up
// its CRDs, webhook Services, and component endpoints across LATER helmfile
// phases, so a "not yet present / 0 endpoints" check is in-progress, not a
// terminal failure. Returning in-progress keeps the converge loop polling
// (riding its budget) until the cluster advances past phase1 — instead of
// tripping the hard-failed-twice abort on infra that is merely still installing
// (observed: an e2e provision gave up at ~18m of a 30m budget on missing
// kyverno-svc / cnpg-webhook / CRDs while apl-core was still installing). A
// cluster genuinely stuck in phase1 still fails — it exhausts the budget. Any
// other code is returned unchanged, so real post-install (non-phase1) failures
// still fail fast.
func PhaseAwareExitCode(code int, phase1 bool) int {
	if phase1 && code == 1 {
		return 2
	}
	return code
}

// ConvergeStep maps a health-check exit code to the loop's next action.
func ConvergeStep(exitCode int) ConvergeAction {
	switch exitCode {
	case 0:
		return ConvergeDone
	case 2:
		return ConvergePoll
	case 1:
		return ConvergeRetryHard
	case 3:
		return ConvergeUnreachable
	default:
		return ConvergeAbort
	}
}
