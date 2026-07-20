// Package health ports check-cluster-health.sh — the single source of truth for
// "is the cluster converged?" (docs/architecture/convergence-contract.md) — into
// testable Go. This file is the convergence contract itself: the accumulator that
// every per-resource check appends to, and the exit-code/verdict it resolves to.
// The per-resource classification predicates (pods, certs, Argo apps, CNPG, jobs,
// …) live alongside it and feed a Report; the kubectl orchestration lives in cmd/llz.
package health

// Verdict is the convergence-contract outcome.
type Verdict int

const (
	// Converged: no hard failures and nothing genuinely in-progress — only
	// operator-deferred and/or cosmetic-drift items remain, or nothing at all.
	Converged Verdict = iota
	// InProgress: a reconcile is still in flight (ImagePulling, cert Issuing,
	// raft retry_join, a Phase-1 OpenBao-bootstrap wait). The caller should poll.
	InProgress
	// HardFailed: a required component is in a state the reconciler can't resolve
	// on its own. The caller should stop and surface to the operator.
	HardFailed
)

// ExitCode is the process exit code the script's callers (converge.sh) branch on:
// 1 hard-failed, 2 in-progress, 0 converged.
func (v Verdict) ExitCode() int {
	switch v {
	case HardFailed:
		return 1
	case InProgress:
		return 2
	default:
		return 0
	}
}

// Report accumulates check outcomes across a health scan. Failed/Pending/Deferred/
// Drift mirror the script's FAILED/PENDING/DEFERRED/DRIFT buckets; pass and warn
// are informational only and do not affect the verdict.
type Report struct {
	Failed   []string
	Pending  []string
	Deferred []string
	Drift    []string

	// RedisAuthSplit is set when at least one Argo Application ComparisonErrors
	// with a repo-server↔argocd-redis auth code (WRONGPASS/NOAUTH). It does not
	// affect the verdict — the split classifies as Pending (poll) — but signals
	// the convergence loop to self-heal by restarting argocd-redis once, so a
	// split that surfaces mid-converge is repaired instead of polling to budget
	// exhaustion. See health.IsRepoServerCacheAuthError and runConverge.
	RedisAuthSplit bool

	// AnnotationLimitWedge is set when at least one Argo Application's sync failed
	// on the 256KB metadata.annotations limit. Like RedisAuthSplit it classifies
	// as Pending (poll), and signals the convergence loop to self-heal by stripping
	// the oversized CRD last-applied-configuration annotation once. See
	// health.IsAnnotationLimitError and runConverge.
	AnnotationLimitWedge bool

	// TunnelDown is set when at least one check failed because the konnectivity
	// tunnel (apiserver → pod) was unavailable. Like the two above it classifies as
	// Pending (poll) rather than a hard failure — the tunnel is Linode-managed
	// infrastructure that heals on its own, so a blip must not burn the converge
	// budget's hard-strike allowance. It is also printed as a single line in the
	// summary, because one dead tunnel fails EVERY apiserver→pod check at once and
	// the unaggregated list reads like several unrelated component faults.
	// See health.IsTunnelBlocked.
	TunnelDown bool

	// GitAuthFailure is set when at least one Argo Application ComparisonErrors
	// because the git remote refused its credential. It is the ODD ONE OUT among
	// the flags above: those three mark transient conditions that classify as
	// Pending, this one marks a terminal condition, and its job is to VETO the
	// phase1 hard-fail downgrade.
	//
	// phase1 demotes failures on the premise that the support plane is merely
	// "still installing". That premise is false here — no helmfile phase mints a
	// git credential — so the demotion turns a knowable abort into a full budget
	// of polling. See health.IsGitAuthError and healthExitCode.
	GitAuthFailure bool
}

// AddFail records a hard failure (a required component the reconciler can't fix).
func (r *Report) AddFail(msg string) { r.Failed = append(r.Failed, msg) }

// AddPending records a genuine reconcile-in-progress that resolves on its own.
func (r *Report) AddPending(msg string) { r.Pending = append(r.Pending, msg) }

// AddDeferred records an operator-deferred external input (a DNS token, a built
// image, …) — healthy-but-waiting, never resolves during a poll loop.
func (r *Report) AddDeferred(msg string) { r.Deferred = append(r.Deferred, msg) }

// AddDrift records a cosmetic OutOfSync on an otherwise-Healthy workload (no exit
// impact — purely informational in the summary).
func (r *Report) AddDrift(msg string) { r.Drift = append(r.Drift, msg) }

// Verdict applies the contract priority: hard failures dominate, then genuine
// in-progress; an operator-deferred-only (or empty) report IS converged — so the
// gate doesn't time out on inputs that won't arrive mid-run.
func (r *Report) Verdict() Verdict {
	switch {
	case len(r.Failed) > 0:
		return HardFailed
	case len(r.Pending) > 0:
		return InProgress
	default:
		return Converged
	}
}

// ExitCode is the convenience for Verdict().ExitCode().
func (r *Report) ExitCode() int { return r.Verdict().ExitCode() }

// Category is the outcome of classifying a single resource — the per-check
// channel a classifier routes to (distinct from the whole-report Verdict).
type Category int

const (
	CatOK       Category = iota // healthy/converged — not tracked (pass)
	CatWarn                     // informational only — printed, never affects the verdict
	CatFail                     // hard failure
	CatPending                  // genuine in-progress
	CatDeferred                 // operator-deferred external input
	CatDrift                    // cosmetic OutOfSync on a Healthy workload
)

// Add routes a categorized finding into the matching Report bucket. CatOK is a
// no-op (passes are not tracked).
func (r *Report) Add(cat Category, msg string) {
	switch cat {
	case CatFail:
		r.AddFail(msg)
	case CatPending:
		r.AddPending(msg)
	case CatDeferred:
		r.AddDeferred(msg)
	case CatDrift:
		r.AddDrift(msg)
	}
}
