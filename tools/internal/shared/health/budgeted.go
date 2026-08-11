package health

// budgeted.go — the same observation means different things to a bounded poll
// and to a one-shot check.
//
// THE REGRESSION THIS EXISTS TO PREVENT. Several states were softened from
// CatFail to CatPending because, on a cluster four minutes old, they are
// indistinguishable from normal startup: a Service with no endpoints, an Argo
// sync that is failing and retrying. That is correct for `llz ci converge`,
// which polls against a BUDGET and reports whatever is still outstanding when it
// runs out — the verdict is deferred, not discarded.
//
// It is WRONG for one-shot `llz ci health`, which is what scheduled
// cluster-health and the in-cluster reconciler run against a steady-state
// cluster. There is no budget there to convert pending back into a verdict, and
// LLZClusterNotConverged fires on llz_convergence_state == 1 — so a permanently
// broken Service or a sync wedged on a policy denial would report "still
// settling" forever and never alert. The softening would have disabled alerting
// on exactly the incident class it was written for.
//
// Set for the duration of a convergence run and restored after, the same way
// converge already borrows kubectlprobe.Retries — see runConverge.
var Budgeted bool

// PendingIfBudgeted returns CatPending when a budget will resolve this state and
// CatFail when nothing will. msgBudgeted is used in the first case; msgFinal —
// which should read as a verdict, not a wait — in the second.
func PendingIfBudgeted(msgBudgeted, msgFinal string) (Category, string) {
	if Budgeted {
		return CatPending, msgBudgeted
	}
	return CatFail, msgFinal
}
