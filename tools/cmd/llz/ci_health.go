package main

// ci_health.go implements `llz ci health` and `llz ci converge` — the native
// ports of check-cluster-health.sh and converge.sh. Every classification is the
// tested internal/health predicate; this file is the kubectl orchestration that
// feeds them and the convergence-contract exit code (1 hard-failed / 2 in-progress
// / 0 converged). `converge` polls `health` until it converges or the budget runs out.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

// healthNamespaces are the namespaces this repo touches — iterated for the
// per-namespace checks (workloads, NetworkPolicies, Services, Leases).
//
// Every loop over this list gates on `if !inv.nsExists[ns] { continue }`, so a
// name that no longer exists is not an error — it is a SILENT SKIP. Three
// entries had gone stale when the platform namespaces were llz- prefixed
// ("openbao", "observability", "cert-automation"), which meant the OpenBao,
// observability, and cert-automation namespaces were never inspected at all:
// no workload check, no default-deny NetworkPolicy check, no Service or Lease
// check. The openbaoNamespace const eight lines below had the correct name the
// whole time.
//
// Keep the llz- prefixed names in sync with the namespaces the components
// actually create (platform-apl/components/*, kubernetes-charts/*). A rename
// here fails open, so it is worth checking against the tree rather than
// assuming.
var healthNamespaces = []string{
	"argocd", "kube-system", "cert-manager", "llz-cert-automation", "external-secrets",
	openbaoNamespace, "llz-observability", "harbor", "istio-system",
}

const openbaoNamespace = "llz-openbao"

func ciHealthCmd() *cobra.Command {
	// failOnUnhealthy defaults true so a bare `llz ci health` keeps its
	// convergence-contract exit semantics (existing callers unchanged). Passing
	// --fail-on-unhealthy=false is REPORT-ONLY: it still runs every check and
	// prints the report, but always exits 0. That lets a shell-less caller (the
	// distroless llz image — no /bin/sh, so no `… || true`) choose report vs gate
	// with a plain value flag instead of a shell conditional. See the
	// clusterHealthWorkflow component's WorkflowTemplate.
	failOnUnhealthy := true
	c := &cobra.Command{
		Use:   "health",
		Short: "cluster convergence health check (exit 0 converged / 2 in-progress / 1 hard-failed / 3 unreachable)",
		Long: "Native port of check-cluster-health.sh — the single source of truth for \"is\n" +
			"the cluster converged?\". Runs every in-cluster check (foundations, OpenBao,\n" +
			"cert-manager, ESO, Argo apps, workloads, storage, jobs, …) against the cluster\n" +
			"$KUBECONFIG points at, classifying each via the unit-tested internal/health\n" +
			"predicates, and exits per the convergence contract: 1 hard-failed, 2 in-\n" +
			"progress (poll), 0 converged, 3 apiserver unreachable (an infrastructure\n" +
			"transient, retried against the budget — never a hard strike).\n\n" +
			"--fail-on-unhealthy=false → report-only: run the checks + print the report but\n" +
			"always exit 0 (for a report-only scheduled run on a shell-less image).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			code := healthExitCode()
			if !failOnUnhealthy {
				if code != 0 {
					fmt.Fprintf(os.Stderr, "::notice::health exit %d suppressed (--fail-on-unhealthy=false, report-only)\n", code)
				}
				return nil // exit 0
			}
			// DELIBERATE os.Exit, not a returned error: every code in the
			// convergence contract is load-bearing and callers DISTINGUISH them.
			// `llz ci converge` (runConverge → health.ConvergeStep) branches on
			// 0 converged / 1 hard-failed / 2 in-progress (keep polling) /
			// 3 apiserver unreachable (retry without spending a hard strike).
			// Returning an error would collapse 2 and 3 into cobra's exit 1 and
			// turn every transient into an immediate hard failure.
			os.Exit(code)
			return nil
		},
	}
	c.Flags().BoolVar(&failOnUnhealthy, "fail-on-unhealthy", true,
		"exit non-zero per the convergence contract on an unhealthy cluster; =false is report-only (always exit 0)")
	return c
}

func ciConvergeCmd() *cobra.Command {
	var budget, interval, retryDelay int
	c := &cobra.Command{
		Use:   "converge",
		Short: "poll `llz ci health` until the cluster converges or the budget runs out",
		Long: "Native port of converge.sh. Polls `llz ci health` (exit 0/1/2/3): converged\n" +
			"-> exit 0; in-progress -> sleep --interval and re-run until --budget elapses\n" +
			"(then exit 1); hard-failed -> re-run once after --retry-delay to absorb a\n" +
			"transient, and exit 1 only if it hard-fails twice in a row; apiserver\n" +
			"unreachable -> re-run after --retry-delay against the budget without spending\n" +
			"a hard strike (a blip can't trip the twice-in-a-row abort).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runConverge(budget, interval, retryDelay)
		},
	}
	c.Flags().IntVar(&budget, "budget", 1800, "total elapsed-time budget in seconds")
	c.Flags().IntVar(&interval, "interval", 30, "seconds between in-progress polls")
	c.Flags().IntVar(&retryDelay, "retry-delay", 60, "seconds before re-running a hard-fail check")
	return c
}

// ── converge loop ────────────────────────────────────────────────────────────

// convergeState carries the one piece of poll-to-poll memory a `llz ci converge`
// run keeps: whether phase1 (OpenBao bootstrap pending) has resolved. (An earlier
// per-section memoization was removed — it forced a full confirm-on-DONE pass
// [~a whole extra health scan] on every converge to guard against masking a
// regression, and once the harbor-kick / store-recovery work made converge hit
// green on the FIRST poll, that confirm was pure cost with no multi-poll benefit
// to recoup it. Each poll now just runs the full health scan; the elapsed-aware
// convergeSleep keeps the pacing cheap.)
type convergeState struct {
	// phase1Done: phase1 resolved FALSE once — the platform-app-ca /
	// ClusterSecretStore probes (each up to 3 tries with 3s pauses) never need to
	// run again this converge (leaving phase1 is one-way within a bootstrap).
	phase1Done bool
}

func newConvergeState() *convergeState { return &convergeState{} }

// convergeSleep is the pause after an in-progress poll: the remainder of
// --interval after the poll's own duration. A full health pass costs tens of
// seconds of kubectl round-trips — sleeping a flat interval ON TOP of that
// (the old behavior) meant a cluster ready mid-cycle waited out both. A poll
// that already took ≥interval proceeds immediately: its work IS the pacing.
func convergeSleep(interval, elapsed time.Duration) time.Duration {
	if remaining := interval - elapsed; remaining > 0 {
		return remaining
	}
	return 0
}

// longPoleCandidates returns the labels keeping a report in-progress (Pending +
// Failed). Pure — the report's tolerated categories (Drift/Deferred) are excluded
// because they do not hold up convergence.
func longPoleCandidates(r *health.Report) []string {
	out := make([]string, 0, len(r.Pending)+len(r.Failed))
	out = append(out, r.Pending...)
	out = append(out, r.Failed...)
	return out
}

// reportConvergeLongPole emits, on convergence, what was still not-OK on the last
// in-progress poll — the tail that gated the run. Best-effort: a notice line plus
// a step-summary section so it lands alongside the phase timeline. No prior
// in-progress poll (converged on the first look) reports a clean fast-path.
func reportConvergeLongPole(prevNonOK []string, prevAttempt int) {
	if len(prevNonOK) == 0 {
		fmt.Fprintln(os.Stderr, "::notice::converge long-pole: none — converged on the first full poll")
		return
	}
	fmt.Fprintf(os.Stderr, "::notice::converge long-pole (still not-OK on poll %d, the last before convergence): %s\n",
		prevAttempt, strings.Join(prevNonOK, "; "))
	var b strings.Builder
	fmt.Fprintf(&b, "### converge long-pole\n\nLast items to go healthy (still not-OK on poll %d):\n\n", prevAttempt)
	for _, item := range prevNonOK {
		fmt.Fprintf(&b, "- %s\n", item)
	}
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", b.String()); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::converge long-pole: step-summary write failed (ignored): %v\n", err)
	}
}

// runConverge polls `health` to a verdict. Unlike `health` itself, converge has
// only a BOOLEAN outcome — converged or not — so it returns nil/error and lets
// cobra own the exit-1. (It still CONSUMES health's full 0/1/2/3 contract
// internally via health.ConvergeStep; only its own result is binary.) The
// ::error:: annotations stay direct stderr writes: GitHub parses an annotation
// only at the start of a line, and a returned error is printed behind main.go's
// "llz: " prefix.
func runConverge(budget, interval, retryDelay int) error {
	deadline := time.Now().Add(time.Duration(budget) * time.Second)
	st := newConvergeState()
	// The converge loop itself is the retry for the cluster probes — a transient
	// blip misread on one poll is corrected ~interval later, and a probe that
	// cannot be answered now records CatPending (kubectl_probe.go), which keeps
	// polling rather than resolving. So don't also pay every probe's internal
	// 3×3s retry pauses on every poll. (Restored on return so one-shot
	// `llz ci health` semantics — and tests — keep the retrying probes.)
	prevProbeRetries := probeRetries
	probeRetries = 1
	defer func() { probeRetries = prevProbeRetries }()
	// Long-pole tracking (Tier-3 instrumentation): remember which apps/resources
	// were still not-OK on the most recent in-progress poll, so on convergence we
	// can report what was the LAST thing to go healthy — confirming the tail's
	// identity across runs instead of assuming it.
	var prevNonOK []string
	var prevAttempt int
	redisRealigned := false
	crdAnnotationsStripped := false
	for attempt := 1; ; attempt++ {
		fmt.Fprintf(os.Stderr, "::notice::convergence poll attempt %d\n", attempt)
		pollStart := time.Now()
		res := healthExitCodeState(st)
		step := health.ConvergeStep(res.code)
		pollDur := time.Since(pollStart)
		// Self-heal a repo-server↔argocd-redis auth split. The redis pod bakes its
		// --requirepass from the argocd-redis Secret at pod start and never re-reads
		// it; on a reused (KEEP_CLUSTER) cluster the Secret can be rewritten (apl-core
		// regenerates it) after redis starts, so freshly-rolled clients read the new
		// password while redis still serves the old one — every app ComparisonErrors
		// with WRONGPASS and converge would otherwise poll until the budget runs out.
		// Restarting redis makes it re-read the current Secret, realigning it with the
		// clients. Once per run: a single restart repairs the split; if it doesn't the
		// budget still bounds the poll and we fail as before (no worse than not trying).
		// This complements the bootstrap workflow's one-shot pre-converge realign,
		// which misses a split that only surfaces during this wait.
		if res.redisAuthSplit && !redisRealigned {
			redisRealigned = true
			realignArgocdRedis()
		}
		// Self-heal a 256KB metadata.annotations wedge. A CRD carrying an oversized
		// client-side last-applied-configuration annotation (a reused-cluster stale
		// copy, or an inherently-large schema like Kyverno's policy CRDs / Gateway-API
		// httproutes) fails EVERY apply to it — including apl-core's own SSA sync — so
		// the owning Application never converges. Stripping that dead-weight annotation
		// (SSA never writes it) unwedges the apply. Once per run; if it doesn't clear,
		// the budget still bounds the poll. Mirrors the bootstrap's proactive step 1b
		// for a wedge that only surfaces during this wait.
		if res.annotationWedge && !crdAnnotationsStripped {
			crdAnnotationsStripped = true
			fmt.Fprintln(os.Stderr, "::warning::an Argo sync hit the 256KB annotation limit — stripping oversized CRD last-applied-configuration annotations")
			stripOversizedCRDLastApplied(kubectlBoolViaExec)
		}
		switch step {
		case health.ConvergeDone:
			// Every poll is a full health scan (no memoized skips), so the DONE
			// verdict already rests on a complete pass — no confirm needed.
			reportConvergeLongPole(prevNonOK, prevAttempt)
			return nil
		case health.ConvergePoll:
			prevNonOK, prevAttempt = res.nonOK, attempt
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "::error::budget of %ds exhausted with the cluster still in-progress.\n", budget)
				return fmt.Errorf("budget of %ds exhausted with the cluster still in-progress", budget)
			}
			time.Sleep(convergeSleep(time.Duration(interval)*time.Second, pollDur))
		case health.ConvergeRetryHard:
			fmt.Fprintf(os.Stderr, "::warning::hard failure reported — re-checking after %ds to absorb transients.\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			if health.ConvergeStep(healthExitCodeState(st).code) == health.ConvergeRetryHard {
				fmt.Fprintln(os.Stderr, "::error::cluster hard-failed twice in a row — operator intervention required.")
				return fmt.Errorf("cluster hard-failed twice in a row — operator intervention required")
			}
			// recovered to converged/in-progress — keep polling
		case health.ConvergeUnreachable:
			// The apiserver was unreachable — an infrastructure transient, not a
			// cluster verdict. Retry against the budget WITHOUT spending a hard
			// strike, so a konnectivity/apiserver blip on one poll can't combine
			// with a later real hard-fail to trip the twice-in-a-row abort. A
			// genuinely unreachable cluster simply exhausts the budget below.
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "::error::budget of %ds exhausted with the apiserver still unreachable — check KUBECONFIG and cluster reachability.\n", budget)
				return fmt.Errorf("budget of %ds exhausted with the apiserver still unreachable — check KUBECONFIG and cluster reachability", budget)
			}
			fmt.Fprintf(os.Stderr, "::warning::apiserver unreachable — transient; re-checking after %ds (not counted as a hard failure).\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
		default:
			fmt.Fprintln(os.Stderr, "::error::health check returned an exit code outside the 0/1/2/3 contract.")
			return fmt.Errorf("health check returned an exit code outside the 0/1/2/3 contract")
		}
	}
}

// realignArgocdRedis restarts the argocd-redis Deployment so it re-reads the
// current argocd-redis Secret password, repairing a repo-server↔redis auth split
// (WRONGPASS/NOAUTH). This is the in-cluster complement to the bootstrap
// workflow's one-shot pre-converge realign: that step only fires if the split is
// already visible when it runs, so a split that surfaces *during* the converge
// wait (a mid-poll Secret rotation) would otherwise go unrepaired. Best-effort:
// failures are logged, never fatal — the convergence budget still bounds the poll
// if the restart doesn't take.
func realignArgocdRedis() {
	fmt.Fprintln(os.Stderr, "::warning::argocd-redis auth split (WRONGPASS/NOAUTH) detected — restarting argocd-redis to re-read the current password")
	if out, err := execOutput("kubectl", "-n", "argocd", "rollout", "restart", "deploy/argocd-redis"); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::argocd-redis rollout restart failed (%v): %s\n", err, strings.TrimSpace(string(out)))
		return
	}
	if _, err := execOutput("kubectl", "-n", "argocd", "rollout", "status", "deploy/argocd-redis", "--timeout=120s"); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::argocd-redis rollout status wait failed (%v) — continuing to poll\n", err)
	}
}

// ── health orchestrator ──────────────────────────────────────────────────────

// healthResult is one health scan's verdict plus the signals runConverge acts on.
// These were three package globals ("last…") that the scan assigned at its tail
// and the converge loop read immediately after — an invariant ("a full scan
// always assigns them, so they never go stale") that had to be re-established by
// prose on every early-return path. Returning them makes the invariant structural:
// every return builds a complete result.
type healthResult struct {
	code int
	// nonOK is the scan's Pending+Failed labels — the convergence long pole.
	nonOK []string
	// redisAuthSplit: a repo-server↔argocd-redis auth split (WRONGPASS/NOAUTH) was
	// seen; runConverge self-heals by restarting argocd-redis once.
	redisAuthSplit bool
	// annotationWedge: an Argo app sync failed on the 256KB metadata.annotations
	// limit; runConverge self-heals by stripping the oversized CRD annotation once.
	annotationWedge bool
}

// healthExitCode runs every check against $KUBECONFIG, prints the report, and
// returns the convergence-contract exit code (0/2/1).
func healthExitCode() int { return healthExitCodeState(nil).code }

// healthExitCodeState is healthExitCode with optional converge state: nil for a
// one-shot `llz ci health`, non-nil inside `llz ci converge`, where the only
// carried fact is phase1Done (so the phase1 probes resolve once per run, not per
// poll). Every poll runs the full set of checks below.
func healthExitCodeState(st *convergeState) healthResult {
	if !kubectlReachable() {
		// Exit 3 (not 1): an unreachable apiserver is an infrastructure transient,
		// not a cluster hard-failure. The converge loop retries it against the
		// budget instead of counting it as a hard strike (see runConverge).
		fmt.Fprintln(os.Stderr, "::error::kubectl cannot reach the apiserver — check KUBECONFIG and cluster reachability.")
		return healthResult{code: 3}
	}

	inv := scanCRDs()

	// Phase 0: pre-bootstrap (Argo CRD / platform-bootstrap App not present yet)
	// is in-progress, not converged — poll. Gated on the CRD list alone, before
	// the namespace fetch: a pre-bootstrap cluster is polled every interval for
	// the whole apl-core helmfile run, and it has nothing for the per-namespace
	// sections to look at yet.
	if !inv.crds["applications.argoproj.io"] ||
		!kExists("-n", "argocd", "get", "application", "platform-bootstrap") {
		fmt.Println(bold("== pre-bootstrap phase detected — apl-core helmfile likely still running =="))
		fmt.Printf("  %s applications.argoproj.io CRD or platform-bootstrap Application not yet present\n", cyan("PENDING"))
		return healthResult{code: 2}
	}

	if !inv.addNamespaces() {
		// Exit 3, same as an unreachable apiserver: kubectlReachable() has already
		// passed, so a failed namespace list is a transient, not a verdict. Reading
		// it as "no namespaces exist" would skip every per-namespace section and
		// report a broken cluster as converged.
		fmt.Fprintln(os.Stderr, "::error::kubectl could not list namespaces — treating as an apiserver transient, not an empty cluster.")
		return healthResult{code: 3}
	}
	// Phase 1: cluster-bootstrap ran but bootstrap-openbao has not completed yet.
	// Historically this was keyed only on cert-manager/platform-app-ca being absent,
	// but apl-core 5.x no longer emits that Secret while the replacement CA chain can
	// already be healthy. Once the openbao ClusterSecretStore is Ready, OpenBao has
	// been unsealed/configured and later failures must fail fast instead of being
	// masked as "still installing" until the converge budget expires. Leaving
	// phase1 is one-way within a bootstrap, so a converge run resolves it once
	// (st.phase1Done) instead of re-paying the probes every poll.
	phase1 := false
	if st == nil || !st.phase1Done {
		phase1 = phase1OpenBaoBootstrapPending()
		if st != nil && !phase1 {
			st.phase1Done = true
		}
	}

	var r health.Report
	checkNodes(&r)
	checkNamespaces(&r, inv)
	checkAPIServices(&r)
	checkRequiredCRDs(&r, inv)
	checkStorageClasses(&r)
	checkFirewallBootstrap(&r)
	checkOpenBao(&r, phase1)
	checkReadyResources(&r, phase1)
	checkWebhooks(&r)
	checkAppProjects(&r, inv)
	checkLeases(&r, inv)
	checkArgoApps(&r, phase1)
	checkWorkloads(&r, inv, phase1)
	checkPVCs(&r)
	checkPVs(&r)
	checkNetworkPolicies(&r, inv)
	checkJobs(&r, phase1)
	checkCronWorkflows(&r, inv)
	checkServices(&r, inv, phase1)
	checkPDBs(&r, phase1)
	checkIngresses(&r, phase1)
	checkWorkflows(&r, inv, phase1)
	checkStuckFinalizers(&r, inv)
	checkPods(&r, phase1)

	printHealthSummary(&r)

	// In phase1 the support plane is still installing (apl-core's CRDs, webhook
	// Services, and endpoints land in later helmfile phases), so a hard-fail here
	// is "not yet installed", not terminal — downgrade it to in-progress so
	// converge keeps polling until the cluster advances past phase1 instead of
	// aborting on still-installing infra. See health.PhaseAwareExitCode.
	code := health.PhaseAwareExitCode(r.ExitCode(), phase1)
	if phase1 && code != r.ExitCode() {
		fmt.Println(bold("== phase1 (support plane still installing) — hard failures above are treated as in-progress; converge will keep polling =="))
	}
	return healthResult{
		code: code,
		// The still-converging set for the converge long-pole report (Tier-3
		// instrumentation): Pending + Failed are the categories that keep the
		// cluster in-progress (Drift/Deferred are tolerated-as-converged), so they
		// are the candidates for "last thing to go healthy".
		nonOK:           longPoleCandidates(&r),
		redisAuthSplit:  r.RedisAuthSplit,
		annotationWedge: r.AnnotationLimitWedge,
	}
}

func printHealthSummary(r *health.Report) {
	fmt.Println()
	for _, c := range r.Drift {
		fmt.Println("  " + yellow("drift:   ") + " " + c)
	}
	for _, c := range r.Deferred {
		fmt.Println("  " + cyan("deferred:") + " " + c)
	}
	for _, c := range r.Pending {
		fmt.Println("  " + cyan("pending: ") + " " + c)
	}
	for _, c := range r.Failed {
		fmt.Println("  " + red("FAILED:  ") + " " + c)
	}
	// One dead tunnel fails every apiserver→pod check at once. Name it as the single
	// cause it is — otherwise the reader sees N unrelated component failures sitting
	// directly under a green "konnectivity-agent (3/3)" line and debugs the symptoms.
	if r.TunnelDown {
		fmt.Println(yellow("  konnectivity tunnel (apiserver → pod) unavailable — the checks above that " +
			"depend on it are inconclusive, not failed. konnectivity-agent reporting Ready does not " +
			"prove the tunnel: its readiness probe does not exercise the dial-out."))
	}
	switch r.Verdict() {
	case health.HardFailed:
		fmt.Printf("%s\n", red(fmt.Sprintf("%d check(s) hard-failed.", len(r.Failed))))
	case health.InProgress:
		fmt.Println(yellow("Cluster is still converging — re-run after a backoff."))
	default:
		if len(r.Deferred) > 0 {
			fmt.Printf("%s %s\n", green("✓"), fmt.Sprintf("Cluster converged — %d operator-deferred item(s) remain, platform healthy.", len(r.Deferred)))
		} else {
			fmt.Printf("%s Cluster converged.\n", green("✓"))
		}
	}
}

// ── kubectl helpers ──────────────────────────────────────────────────────────

func kubectlReachable() bool {
	_, err := execOutput("kubectl", "version", "--request-timeout=10s")
	return err == nil
}

func phase1OpenBaoBootstrapPending() bool {
	// kExists retries an unanswerable probe (kubectl_probe.go), so a transient
	// API/ACL blip no longer reads as a missing CA and mislabels the phase.
	if kExists("-n", "cert-manager", "get", "secret", "platform-app-ca") {
		return false
	}
	return !openBaoClusterSecretStoreReadyWithRetry()
}

func openBaoClusterSecretStoreReadyWithRetry() bool {
	for attempt := 0; attempt < probeRetries; attempt++ {
		if openBaoClusterSecretStoreReady() {
			return true
		}
		if attempt < probeRetries-1 {
			time.Sleep(probeDelay)
		}
	}
	return false
}

func openBaoClusterSecretStoreReady() bool {
	out, err := execOutput("kubectl", "get", "clustersecretstore", defaultSecretStore, "-o", "json")
	if err != nil {
		return false
	}
	var item readyResourceItem
	if err := json.Unmarshal(out, &item); err != nil {
		return false
	}
	status, _, _ := health.FindReady(item.Status.Conditions)
	return status == "True"
}

// sectionItems fetches a section's corpus and, when the cluster did not answer,
// records an inconclusive finding instead of letting the section iterate an
// empty list and report green. This is requireCorpus for cluster probes: a
// section that had nothing to check otherwise prints the same clean run as one
// that checked everything. CatPending (not CatFail) because converge's poll loop
// should re-ask — an unreadable cluster is not converged, but it is also not
// proof of a broken one; the budget decides.
func sectionItems[T any](r *health.Report, kind string, args ...string) []T {
	items, ok := kListOK[T](args...)
	if !ok {
		record(r, health.CatPending, "could not list "+kind+" — cluster read failed after retries; treating as inconclusive rather than 'none found'")
		return nil
	}
	return items
}

// clusterInventory is one scan's snapshot of the two cluster-wide name lists the
// sections consult over and over: which CRDs are installed, and which namespaces
// exist. Each of those questions used to be its own kubectl process — ~21 for the
// required-CRD section, up to 9 more for the stuck-finalizer kinds, plus one
// namespace probe per namespace per per-namespace section (workloads,
// NetworkPolicies, Services, Leases) — roughly 60 spawns a pass, the bulk of the
// "tens of seconds of kubectl round-trips" convergeSleep exists to absorb. Two
// list calls answer all of them with the same semantics the per-name probes had:
// a name absent from the list — or a list that failed outright — reads as "not
// present", exactly as a failed `kubectl get <kind> <name>` did.
type clusterInventory struct {
	crds     map[string]bool
	nsExists map[string]bool
	// namespaces is the same namespace list with .status.phase, so checkNamespaces
	// reuses the fetch it was already paying for instead of adding a second one.
	namespaces []namespaceItem
}

// The namespace list is fetched with its success reported, because a failed call
// here would FAIL OPEN. checkLeases/checkWorkloads/checkNetworkPolicies/
// checkServices are all skip-if-absent: they consult nsExists and quietly do
// nothing for a namespace that is not there. Collapsing nine independent
// `get ns <name>` probes into one list means one dropped call empties nsExists
// and silently removes ALL FOUR sections from the pass — a hard-failed workload
// would report converged. The old per-name probes needed nine simultaneous
// failures to lose the same coverage.
//
// So an errored namespace list is not data: ok=false, and the caller returns
// exit 3 (apiserver transient) so converge retries against its budget rather
// than banking a false green. The CRD list needs no such handling — a failed one
// empties inv.crds, which trips the phase-0 gate into exit 2 and short-circuits
// before any CRD-driven section runs.
// scanCRDs takes the first of the two list calls. A failed one empties inv.crds,
// which trips the phase-0 gate into exit 2 — loud enough, and the same posture
// the per-name CRD probes had, since that same CRD gated phase-0 by name before.
func scanCRDs() *clusterInventory {
	inv := &clusterInventory{crds: map[string]bool{}, nsExists: map[string]bool{}}
	for _, crd := range kList[meta]("get", "crd") {
		inv.crds[crd.Metadata.Name] = true
	}
	return inv
}

// addNamespaces takes the second, and reports whether the call actually
// succeeded. Called after the phase-0 gate so a pre-bootstrap poll — which has
// no namespaces worth listing — does not pay for it every interval.
func (inv *clusterInventory) addNamespaces() bool {
	raw, ok := kItemsOK("get", "ns")
	if !ok {
		return false
	}
	inv.namespaces = decodeItems[namespaceItem](raw)
	for _, ns := range inv.namespaces {
		inv.nsExists[ns.Metadata.Name] = true
	}
	return true
}

// scanInventory is both halves, for callers that want the whole snapshot.
func scanInventory() (*clusterInventory, bool) {
	inv := scanCRDs()
	return inv, inv.addNamespaces()
}

// catStyles renders a health category's report label: the fixed-width text and
// the severity tint (which degrades to plain off a TTY — color.go). A package
// table rather than a per-call map literal: record fires once per node, CRD, app,
// workload, pod, PVC and Service — hundreds of times a scan, every interval under
// converge. An unknown category falls through to a blank, uncolored label.
var catStyles = map[health.Category]struct {
	label string
	color func(string) string
}{
	health.CatOK:       {"OK", green},
	health.CatWarn:     {"WARN", yellow},
	health.CatFail:     {"FAIL", red},
	health.CatPending:  {"PENDING", cyan},
	health.CatDeferred: {"DEFERRED", cyan},
	health.CatDrift:    {"DRIFT", yellow},
}

// record prints a labeled line for a finding and routes it into the report
// (CatOK/CatWarn print but never affect the verdict).
func record(r *health.Report, cat health.Category, msg string) {
	// A hard failure whose text is the konnectivity signature is an apiserver→pod
	// transport outage, not a verdict on the component — downgrade it to Pending so
	// converge polls its budget instead of spending a hard strike. Done here, at the
	// single funnel every check routes through, so it covers each apiserver→pod
	// surface (APIService discovery, exec-based probes) without touching them
	// individually. See health.IsTunnelBlocked.
	if cat == health.CatFail && health.IsTunnelBlocked(msg) {
		r.TunnelDown = true
		cat = health.CatPending
	}
	style := catStyles[cat]
	// Pad to the fixed column on the PLAIN label, then color — the ANSI escapes are
	// zero-width, so the columns stay aligned (color.go).
	label := fmt.Sprintf("%-8s", style.label)
	if style.color != nil {
		label = style.color(label)
	}
	fmt.Printf("  %s %s\n", label, msg)
	r.Add(cat, msg)
}

func hdr(s string) { fmt.Printf("\n%s\n", bold("== "+s+" ==")) }

// metaName / nsName extract common metadata for inline-typed items.
type meta struct {
	Metadata struct {
		Namespace         string            `json:"namespace"`
		Name              string            `json:"name"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Finalizers        []string          `json:"finalizers"`
	} `json:"metadata"`
}

// ── sections ─────────────────────────────────────────────────────────────────

func checkNodes(r *health.Report) {
	hdr("node health")
	for _, n := range sectionItems[health.Node](r, "Nodes", "get", "nodes") {
		ok, ready, mem, disk, pid := health.NodeHealthy(n)
		if ok {
			record(r, health.CatOK, fmt.Sprintf("Node %s (Ready, no pressure)", n.Name()))
		} else {
			record(r, health.CatFail, fmt.Sprintf("Node %s (Ready=%s MemPressure=%s DiskPressure=%s PIDPressure=%s)", n.Name(), ready, mem, disk, pid))
		}
		for _, t := range health.UnexpectedTaints(n) {
			val := ""
			if t.Value != "" {
				val = "=" + t.Value
			}
			record(r, health.CatFail, fmt.Sprintf("Node %s has unexpected taint %s%s:%s (blocks scheduling)", n.Name(), t.Key, val, t.Effect))
		}
	}
}

// namespaceItem is a namespace with the phase checkNamespaces judges; the same
// fetch also backs clusterInventory.nsExists.
type namespaceItem struct {
	meta
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func checkNamespaces(r *health.Report, inv *clusterInventory) {
	hdr("namespaces (stuck Terminating)")
	stuck := false
	for _, ns := range inv.namespaces {
		if health.NamespaceTerminating(ns.Status.Phase) {
			record(r, health.CatFail, fmt.Sprintf("Namespace %s stuck Terminating (check .spec.finalizers and stuck CRs)", ns.Metadata.Name))
			stuck = true
		}
	}
	if !stuck {
		record(r, health.CatOK, "no namespaces in Terminating state")
	}
}

func checkAPIServices(r *health.Report) {
	hdr("APIService availability")
	for _, a := range sectionItems[health.APIService](r, "APIServices", "get", "apiservices") {
		if bad, msg := health.APIServiceUnavailable(a); bad {
			record(r, health.CatFail, fmt.Sprintf("APIService %s not Available — %s", a.Metadata.Name, msg))
		}
	}
}

func checkRequiredCRDs(r *health.Report, inv *clusterInventory) {
	hdr("required CRDs")
	for _, crd := range health.RequiredCRDs() {
		if inv.crds[crd] {
			record(r, health.CatOK, "CRD "+crd+" installed")
		} else {
			record(r, health.CatFail, "CRD "+crd+" missing — owning ArgoCD Application has not installed it")
		}
	}
}

func checkStorageClasses(r *health.Report) {
	hdr("StorageClasses")
	for _, sc := range health.RequiredStorageClasses() {
		if kExists("get", "storageclass", sc) {
			record(r, health.CatOK, "StorageClass "+sc+" present")
		} else {
			record(r, health.CatFail, "StorageClass "+sc+" missing")
		}
	}
	classes := sectionItems[health.StorageClass](r, "StorageClasses", "get", "storageclass")
	switch def := health.DefaultStorageClasses(classes); len(def) {
	case 1:
		record(r, health.CatOK, "exactly one default StorageClass ("+def[0]+")")
	case 0:
		record(r, health.CatFail, "no default StorageClass — PVCs without an explicit storageClassName will stay Pending")
	default:
		// Two defaults is the transient cold-start state, NOT a terminal failure:
		// LKE's Flux-managed workload HelmRelease ships linode-block-storage-retain
		// as a default, and the sc-demote reconciler (leader-gated, watch + resync
		// floor) demotes it so block-storage-retain is the sole default. On a fresh
		// cluster that demote lands within the reconciler's resync floor (~120s),
		// which can exceed a single converge poll's hard-fail tolerance — so classify
		// it as in-progress (poll against the budget) rather than CatFail. A genuinely
		// stuck duplicate (reconciler down/never-leader) still fails, but on budget
		// exhaustion instead of a fast hard-fail that races the self-heal. See
		// reconcile_sc_demote.go + the leader-election re-fire in reconcile.go.
		record(r, health.CatPending, fmt.Sprintf("%d default StorageClasses (%s) — non-deterministic; awaiting sc-demote reconciler", len(def), strings.Join(def, ",")))
	}
}

func checkFirewallBootstrap(r *health.Report) {
	hdr("cloud-firewall bootstrap (kube-system)")
	// The firewall controller is optional (the private llz-linode-cidr-firewall
	// chart + the cidrFirewall component that feeds it). When neither the
	// controller Deployment nor its ConfigMap exists the component is simply not
	// enabled on this instance — skip instead of failing every public adopter.
	// (Before the cidrFirewall component, `llz ci bootstrap-cloud-firewall`
	// seeded the ConfigMap unconditionally on every apply, so its absence WAS a
	// bootstrap failure; now the ConfigMap only exists where the component runs.)
	// This is the one branch where absence means "pass the whole section", so it
	// is the one that must not accept an unanswerable probe as absence: a blip on
	// both reads would skip every firewall check with an OK.
	depExists, depAnswered := kExistsOK("-n", "kube-system", "get", "deployment", firewallDeploymentName)
	cmExists, cmAnswered := kExistsOK("-n", "kube-system", "get", "configmap", firewallConfigMapName)
	if !depAnswered || !cmAnswered {
		record(r, health.CatPending, "could not read kube-system firewall-controller Deployment/ConfigMap — cannot tell 'component disabled' from 'unreadable cluster'")
		return
	}
	if !depExists && !cmExists {
		record(r, health.CatOK, "firewall-controller not installed (cidrFirewall component disabled) — skipped")
		return
	}
	exists := kExists("-n", "kube-system", "get", "secret", "linode")
	token := ""
	if exists {
		token = kJSONPath("-n", "kube-system", "get", "secret", "linode", "-o", "jsonpath={.data.token}")
	}
	cat, msg := health.ClassifyFirewallToken(exists, token)
	record(r, cat, msg)

	// firewallConfigMapName (ci_firewall.go) is the single source of truth for the
	// ConfigMap name the private chart renders (<fullname>-config =
	// llz-linode-cidr-firewall-config) and `bootstrap-cloud-firewall` patches.
	if !kExists("-n", "kube-system", "get", "configmap", firewallConfigMapName) {
		record(r, health.CatFail, "ConfigMap kube-system/"+firewallConfigMapName+" missing")
		return
	}
	record(r, health.CatOK, "ConfigMap kube-system/"+firewallConfigMapName+" exists")
	for _, key := range []string{"LINODE_FIREWALL_ID", "LKE_CLUSTER_ID", "FIREWALL_TEMPLATE_ID", "RECONCILE_INTERVAL_SECS", "VPC_CIDR"} {
		val := kJSONPath("-n", "kube-system", "get", "configmap", firewallConfigMapName, "-o", "jsonpath={.data."+key+"}")
		cat := health.ClassifyFirewallConfigKey(key, val)
		if cat == health.CatOK {
			record(r, health.CatOK, "  "+key+" = "+val)
		} else {
			record(r, health.CatDeferred, "  "+key+" empty (set when the firewall bootstrap / Argo app runs)")
		}
	}
}

func checkReadyResources(r *health.Report, phase1 bool) {
	// cert-manager ClusterIssuers / Certificates / CertificateRequests + ESO.
	readyKind(r, "ClusterIssuer", []string{"get", "clusterissuers.cert-manager.io"}, false,
		func(key string) bool { return phase1 && health.MatchPrefix(key, health.Phase1PendingIssuers()) },
		health.ExternalDepIssuers())
	readyKind(r, "Certificate", []string{"get", "certificates.cert-manager.io", "-A"}, true,
		func(key string) bool { return phase1 && health.MatchPrefix(key, health.Phase1PendingCerts()) },
		health.ExternalDepCerts())
	certRequests(r, phase1)
	readyKind(r, "ClusterSecretStore", []string{"get", "clustersecretstores.external-secrets.io"}, false,
		func(string) bool { return phase1 }, nil)
	readyKind(r, "ExternalSecret", []string{"get", "externalsecrets.external-secrets.io", "-A"}, true,
		func(string) bool { return phase1 }, health.ExternalDepExternalSecrets())
}

// readyResourceItem is a resource with a Ready condition.
type readyResourceItem struct {
	meta
	Status struct {
		Conditions []health.Condition `json:"conditions"`
	} `json:"status"`
}

func readyKind(r *health.Report, kind string, getArgs []string, namespaced bool, phase1Pending func(key string) bool, extDep []health.DepEntry) {
	hdr(kind + "s")
	for _, it := range sectionItems[readyResourceItem](r, kind+"s", getArgs...) {
		key := it.Metadata.Name
		if namespaced {
			key = it.Metadata.Namespace + "/" + it.Metadata.Name
		}
		status, reason, msg := health.FindReady(it.Status.Conditions)
		cat, line := health.ClassifyReady(kind, key, status, reason, msg, phase1Pending(key), extDep)
		record(r, cat, line)
	}
}

func certRequests(r *health.Report, phase1 bool) {
	hdr("CertificateRequests")
	for _, it := range sectionItems[readyResourceItem](r, "CertificateRequests", "get", "certificaterequests.cert-manager.io", "-A") {
		key := it.Metadata.Namespace + "/" + it.Metadata.Name
		status, reason, msg := health.FindReady(it.Status.Conditions)
		p1 := phase1 && health.MatchPrefix(key, health.Phase1PendingCerts())
		cat, line := health.ClassifyCertificateRequest(key, status, reason, msg, p1, health.ExternalDepCerts())
		record(r, cat, line)
	}
}

func checkOpenBao(r *health.Report, phase1 bool) {
	hdr("openbao seal / HA")
	// A CatWarn skip never affects the verdict, so an unreadable STS would retire
	// the entire seal check silently — demand an actual answer before skipping.
	specReplicas, answered := kJSONPathOK("-n", openbaoNamespace, "get", "sts", "platform-openbao", "-o", "jsonpath={.spec.replicas}")
	if !answered {
		record(r, health.CatPending, "could not read openbao/platform-openbao StatefulSet — seal check inconclusive")
		return
	}
	replicas, err := strconv.Atoi(strings.TrimSpace(specReplicas))
	if err != nil || replicas == 0 {
		record(r, health.CatWarn, "OpenBao StatefulSet not present — skipping seal check")
		return
	}
	active := 0
	// Set when a pod's seal state could not be read because the konnectivity tunnel
	// was down. The leader count is DERIVED from those reads, so it must not be
	// judged on them: with all three execs blocked, active stays 0 and the count
	// would hard-fail "no active leader" — a conclusion drawn from three
	// measurements that never happened. Its own text carries no tunnel signature,
	// so record() cannot catch it; the fact has to be threaded here.
	tunnelBlocked := false
	for i := 0; i < replicas; i++ {
		pod := fmt.Sprintf("platform-openbao-%d", i)
		if !kExists("-n", openbaoNamespace, "get", "pod", pod) {
			record(r, health.CatFail, "Pod openbao/"+pod+" missing")
			continue
		}
		ready := kJSONPath("-n", openbaoNamespace, "get", "pod", pod, "-o", `jsonpath={.status.containerStatuses[?(@.name=="openbao")].ready}`)
		if ready != "true" {
			record(r, health.CatPending, "Pod openbao/"+pod+" (openbao container not Ready — can't query seal status)")
			continue
		}
		out, execErr := execOutput("kubectl", "-n", openbaoNamespace, "exec", pod, "-c", "openbao", "--",
			"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true", "bao", "status", "-format=json")
		st, perr := health.ParseBaoStatus(out)
		if perr != nil {
			// `bao status` runs through `kubectl exec`, i.e. the konnectivity tunnel.
			// The exec error was previously discarded, so a dead tunnel surfaced as the
			// unattributable "could not parse bao status JSON" on all three pods —
			// reading as an OpenBao fault when OpenBao was never reached. Carry the
			// stderr into the message: it names the real cause, and lets record()
			// classify a tunnel outage as Pending rather than a hard failure.
			msg := "Pod openbao/" + pod + " (could not parse bao status JSON"
			if detail := strings.TrimSpace(execErrText(execErr)); detail != "" {
				msg += " — " + detail
			}
			record(r, health.CatFail, msg+")")
			tunnelBlocked = tunnelBlocked || health.IsTunnelBlocked(msg)
			continue
		}
		cat, msg := health.ClassifyBaoSeal(st)
		record(r, cat, "Pod openbao/"+pod+" ("+msg+")")
		if cat == health.CatOK && st.HAMode == "active" {
			active++
		}
		if cat == health.CatOK && !phase1 {
			if kExists("-n", openbaoNamespace, "exec", pod, "-c", "openbao", "--", "test", "-s", "/openbao/audit/audit.log") {
				record(r, health.CatOK, "  audit device active on "+pod)
			} else {
				record(r, health.CatFail, "  audit device inactive on "+pod+" — /openbao/audit/audit.log missing or empty")
			}
		}
	}
	if cat, msg := health.ClassifyLeaderCount(replicas, active); cat != health.CatOK {
		if tunnelBlocked && cat == health.CatFail {
			cat = health.CatPending
			msg += " — seal state unread on ≥1 pod (konnectivity tunnel down); leader count inconclusive"
		}
		record(r, cat, msg)
	}
}

// webhookConfigItem is a Validating/MutatingWebhookConfiguration reduced to the
// Service backends whose endpoints decide whether the webhook can be served.
type webhookConfigItem struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Webhooks []struct {
		ClientConfig struct {
			Service *struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"service"`
		} `json:"clientConfig"`
	} `json:"webhooks"`
}

func checkWebhooks(r *health.Report) {
	hdr("admission webhooks (Validating + Mutating)")
	for _, kind := range []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"} {
		for _, cfg := range sectionItems[webhookConfigItem](r, kind, "get", kind) {
			for _, wh := range cfg.Webhooks {
				if wh.ClientConfig.Service == nil {
					continue
				}
				ns, svc := wh.ClientConfig.Service.Namespace, wh.ClientConfig.Service.Name
				if ns == "" || svc == "" {
					continue
				}
				exists := kExists("-n", ns, "get", "svc", svc)
				ready := countReadyEndpoints(ns, svc)
				cat, msg := health.ClassifyWebhookBackend(exists, ready)
				record(r, cat, fmt.Sprintf("%s %s → %s/%s %s", kind, cfg.Metadata.Name, ns, svc, msg))
			}
		}
	}
}

func checkAppProjects(r *health.Report, inv *clusterInventory) {
	hdr("ArgoCD AppProjects")
	if !inv.crds["appprojects.argoproj.io"] {
		return
	}
	// platform-support is the only per-domain AppProject the support-plane
	// Applications reference.
	for _, ap := range []string{"platform-support"} {
		if kExists("-n", "argocd", "get", "appproject", ap) {
			record(r, health.CatOK, "AppProject argocd/"+ap+" present")
		} else {
			record(r, health.CatFail, "AppProject argocd/"+ap+" missing — child Applications will ComparisonError 'project not found'")
		}
	}
}

// leaseItem is a coordination Lease reduced to the renewal fields
// health.LeaseStale judges.
type leaseItem struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity       string `json:"holderIdentity"`
		LeaseDurationSeconds int    `json:"leaseDurationSeconds"`
		RenewTime            string `json:"renewTime"`
	} `json:"spec"`
}

func checkLeases(r *health.Report, inv *clusterInventory) {
	hdr("controller Lease freshness")
	now := time.Now()
	stale := false
	for _, ns := range []string{"argocd", "cert-manager", "external-secrets", "cert-automation", "openbao", "kube-system"} {
		if !inv.nsExists[ns] {
			continue
		}
		for _, it := range sectionItems[leaseItem](r, "Leases in "+ns, "-n", ns, "get", "leases.coordination.k8s.io") {
			if it.Spec.RenewTime == "" {
				continue
			}
			renew, err := time.Parse(time.RFC3339, it.Spec.RenewTime)
			if err != nil {
				continue
			}
			if health.LeaseStale(renew, now, it.Spec.LeaseDurationSeconds) {
				record(r, health.CatFail, fmt.Sprintf("Lease %s/%s stale (holder=%s) — leader-elected controller silently stopped", ns, it.Metadata.Name, it.Spec.HolderIdentity))
				stale = true
			}
		}
	}
	if !stale {
		record(r, health.CatOK, "all controller Leases renewed within 4× leaseDuration")
	}
}

func checkArgoApps(r *health.Report, phase1 bool) {
	hdr("ArgoCD Applications")
	for _, raw := range sectionItems[json.RawMessage](r, "ArgoCD Applications", "-n", "argocd", "get", "applications.argoproj.io") {
		a, err := health.ParseArgoApp(raw)
		if err != nil {
			continue
		}
		cat, msg := health.ClassifyArgoApp(a, phase1)
		record(r, cat, msg)
		// A repo-server↔argocd-redis auth split (WRONGPASS/NOAUTH) makes every app
		// ComparisonError at once; flag it so the converge loop can restart redis
		// once rather than poll to budget exhaustion on a self-inflicted deadlock.
		if health.IsRepoServerCacheAuthError(a.SpecErr) {
			r.RedisAuthSplit = true
		}
		// A sync that failed on the 256KB metadata.annotations limit (an oversized
		// client-side last-applied-configuration on a CRD) wedges every apply to
		// that object; flag it so the converge loop strips the annotation once.
		if health.IsAnnotationLimitError(a.OpErr) {
			r.AnnotationLimitWedge = true
		}
	}
}

// deploymentItem / statefulSetItem / daemonSetItem are the replica counts the
// workload classifiers judge.
type deploymentItem struct {
	meta
	Spec struct {
		Replicas int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int                `json:"readyReplicas"`
		Conditions    []health.Condition `json:"conditions"`
	} `json:"status"`
}

type statefulSetItem struct {
	meta
	Spec struct {
		Replicas int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

type daemonSetItem struct {
	meta
	Status struct {
		DesiredNumberScheduled int `json:"desiredNumberScheduled"`
		NumberReady            int `json:"numberReady"`
		UpdatedNumberScheduled int `json:"updatedNumberScheduled"`
		NumberMisscheduled     int `json:"numberMisscheduled"`
	} `json:"status"`
}

func checkWorkloads(r *health.Report, inv *clusterInventory, phase1 bool) {
	hdr("Deployments / StatefulSets / DaemonSets")
	for _, ns := range healthNamespaces {
		if !inv.nsExists[ns] {
			continue
		}
		for _, d := range sectionItems[deploymentItem](r, "Deployments in "+ns, "-n", ns, "get", "deploy") {
			preason, pmsg := progressingCondition(d.Status.Conditions)
			cat, msg := health.ClassifyWorkload("Deployment", ns, d.Metadata.Name, d.Spec.Replicas, d.Status.ReadyReplicas, preason, pmsg, phase1)
			record(r, cat, msg)
		}
		for _, s := range sectionItems[statefulSetItem](r, "StatefulSets in "+ns, "-n", ns, "get", "sts") {
			cat, msg := health.ClassifyWorkload("StatefulSet", ns, s.Metadata.Name, s.Spec.Replicas, s.Status.ReadyReplicas, "", "", phase1)
			record(r, cat, msg)
		}
		for _, ds := range sectionItems[daemonSetItem](r, "DaemonSets in "+ns, "-n", ns, "get", "ds") {
			cat, msg := health.ClassifyDaemonSet(ns, ds.Metadata.Name, ds.Status.DesiredNumberScheduled, ds.Status.NumberReady, ds.Status.UpdatedNumberScheduled, ds.Status.NumberMisscheduled)
			record(r, cat, msg)
		}
	}
}

// pvcItem / pvItem are the binding phase (and requested class) the storage
// classifiers judge.
type pvcItem struct {
	meta
	Spec struct {
		StorageClassName string `json:"storageClassName"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type pvItem struct {
	meta
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func checkPVCs(r *health.Report) {
	hdr("PersistentVolumeClaim binding")
	for _, p := range sectionItems[pvcItem](r, "PVCs", "get", "pvc", "-A") {
		cat, msg := health.ClassifyPVC(p.Metadata.Namespace, p.Metadata.Name, p.Status.Phase, p.Spec.StorageClassName)
		record(r, cat, msg)
	}
}

func checkPVs(r *health.Report) {
	hdr("PersistentVolume hygiene")
	released := 0
	for _, p := range sectionItems[pvItem](r, "PVs", "get", "pv") {
		switch health.ClassifyPVPhase(p.Status.Phase) {
		case health.CatFail:
			record(r, health.CatFail, fmt.Sprintf("PV %s %s — provisioner/CSI issue; dependent PVC will stay Pending", p.Metadata.Name, p.Status.Phase))
		case health.CatWarn:
			record(r, health.CatWarn, fmt.Sprintf("PV %s unrecognized phase=%s", p.Metadata.Name, p.Status.Phase))
		default:
			if p.Status.Phase == "Released" {
				released++
			}
		}
	}
	if released > 0 {
		record(r, health.CatWarn, fmt.Sprintf("%d Released PV(s) — expected with Retain; run orphan-cleanup so leaked Volumes don't count against quota", released))
	} else {
		record(r, health.CatOK, "no Released/Failed/Pending PVs")
	}
}

func checkNetworkPolicies(r *health.Report, inv *clusterInventory) {
	hdr("NetworkPolicy presence per namespace")
	for _, ns := range healthNamespaces {
		if !inv.nsExists[ns] || health.NetpolExemptNamespace(ns) {
			continue
		}
		cat, msg := health.ClassifyNamespaceNetpol(ns, len(kItems("-n", ns, "get", "networkpolicies")))
		record(r, cat, msg)
	}
}

type jobItem struct {
	Metadata struct {
		Namespace         string            `json:"namespace"`
		Name              string            `json:"name"`
		CreationTimestamp string            `json:"creationTimestamp"`
		OwnerReferences   []health.OwnerRef `json:"ownerReferences"`
	} `json:"metadata"`
	Status struct {
		Succeeded  int                `json:"succeeded"`
		Failed     int                `json:"failed"`
		Active     int                `json:"active"`
		Conditions []health.Condition `json:"conditions"`
	} `json:"status"`
}

func checkJobs(r *health.Report, phase1 bool) {
	hdr("Jobs (failed or stuck)")
	var items []jobItem
	var runs []health.JobRun
	for _, j := range sectionItems[jobItem](r, "Jobs", "get", "jobs", "-A") {
		// Ephemeral e2e exercise Jobs (e.g. broad-pat-rotator-e2e) are judged by
		// their own assert step; a Failed one lingering from a prior run on a
		// reused cluster must not gate convergence. Same rationale as the Workflow
		// scan (ClassifyWorkflowPhase / IsEphemeralE2EProbe).
		if health.IsEphemeralE2EProbe(j.Metadata.Name) {
			continue
		}
		items = append(items, j)
		key := j.Metadata.Namespace + "/" + j.Metadata.Name
		complete, failed := false, false
		for _, c := range j.Status.Conditions {
			if c.Type == "Complete" && c.Status == "True" {
				complete = true
			}
			if c.Type == "Failed" && c.Status == "True" {
				failed = true
			}
		}
		var cronOwner string
		for _, o := range j.Metadata.OwnerReferences {
			if o.Kind == "CronJob" {
				cronOwner = o.Name
			}
		}
		created, _ := time.Parse(time.RFC3339, j.Metadata.CreationTimestamp)
		runs = append(runs, health.JobRun{Key: key, CronOwner: cronOwner, Created: created, Complete: complete, Failed: failed})
	}
	// An early CronJob tick that failed before its backing service was up, then
	// superseded by a later successful tick, must not fail the gate (see
	// health.SupersededFailedJobs).
	superseded := health.SupersededFailedJobs(runs)
	for i, j := range items {
		run := runs[i]
		if run.Failed && !run.Complete && superseded[run.Key] {
			record(r, health.CatOK, "Job "+run.Key+" Failed but superseded by a newer successful "+run.CronOwner+" CronJob run")
			continue
		}
		p1 := phase1 && health.MatchPrefix(run.Key, health.Phase1PendingWorkloads())
		cat, msg := health.ClassifyJob(run.Key, run.Complete, run.Failed, j.Status.Active, j.Status.Succeeded, j.Status.Failed, p1)
		record(r, cat, msg)
	}
}

// cronWorkflowItem is a CronWorkflow reduced to the submission/schedule state
// health.ClassifyCronWorkflow judges.
type cronWorkflowItem struct {
	meta
	Spec struct {
		Suspend bool `json:"suspend"`
	} `json:"spec"`
	Status struct {
		Conditions        []health.Condition `json:"conditions"`
		LastScheduledTime string             `json:"lastScheduledTime"`
	} `json:"status"`
}

func checkCronWorkflows(r *health.Report, inv *clusterInventory) {
	hdr("CronWorkflows")
	if !inv.crds["cronworkflows.argoproj.io"] {
		return
	}
	now := time.Now()
	for _, cw := range sectionItems[cronWorkflowItem](r, "CronWorkflows", "get", "cronworkflows.argoproj.io", "-A") {
		key := cw.Metadata.Namespace + "/" + cw.Metadata.Name
		submissionErr := ""
		for _, c := range cw.Status.Conditions {
			if c.Type == "SubmissionError" {
				submissionErr = c.Message
			}
		}
		ageDays := -1
		if cw.Status.LastScheduledTime != "" {
			if last, err := time.Parse(time.RFC3339, cw.Status.LastScheduledTime); err == nil {
				ageDays = int(now.Sub(last).Hours() / 24)
			}
		}
		cat, msg := health.ClassifyCronWorkflow(key, submissionErr, cw.Spec.Suspend, ageDays, 30)
		record(r, cat, msg)
	}
}

// serviceItem carries the two fields that decide whether a Service is expected
// to have endpoints at all (ExternalName and headless Services are not).
type serviceItem struct {
	meta
	Spec struct {
		Type      string `json:"type"`
		ClusterIP string `json:"clusterIP"`
	} `json:"spec"`
}

func checkServices(r *health.Report, inv *clusterInventory, phase1 bool) {
	hdr("Service endpoints (repo namespaces)")
	for _, ns := range healthNamespaces {
		if !inv.nsExists[ns] {
			continue
		}
		for _, s := range sectionItems[serviceItem](r, "Services in "+ns, "-n", ns, "get", "svc") {
			if s.Spec.Type == "ExternalName" || s.Spec.ClusterIP == "None" {
				continue
			}
			key := ns + "/" + s.Metadata.Name
			p1 := phase1 && health.MatchPrefix(key, health.Phase1PendingWorkloads())
			cat, msg := health.ClassifyServiceEndpoints(key, countReadyEndpoints(ns, s.Metadata.Name), p1)
			if cat != health.CatOK { // only surface non-OK to cut noise (matches script's VERBOSE-gated pass)
				record(r, cat, msg)
			}
		}
	}
}

// pdbItem is a PodDisruptionBudget's healthy/allowed counts.
type pdbItem struct {
	meta
	Status struct {
		CurrentHealthy     int `json:"currentHealthy"`
		DesiredHealthy     int `json:"desiredHealthy"`
		DisruptionsAllowed int `json:"disruptionsAllowed"`
		ExpectedPods       int `json:"expectedPods"`
	} `json:"status"`
}

func checkPDBs(r *health.Report, phase1 bool) {
	hdr("PodDisruptionBudgets")
	for _, p := range sectionItems[pdbItem](r, "PDBs", "get", "pdb", "-A") {
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		cat, msg := health.ClassifyPDB(key, p.Status.CurrentHealthy, p.Status.DesiredHealthy, p.Status.DisruptionsAllowed, p.Status.ExpectedPods, phase1)
		if cat != health.CatOK {
			record(r, cat, msg)
		}
	}
}

// ingressItem is an Ingress reduced to its load-balancer address count.
type ingressItem struct {
	meta
	Status struct {
		LoadBalancer struct {
			Ingress []json.RawMessage `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

func checkIngresses(r *health.Report, phase1 bool) {
	hdr("Ingress addresses")
	for _, ing := range sectionItems[ingressItem](r, "Ingresses", "get", "ingress", "-A") {
		key := ing.Metadata.Namespace + "/" + ing.Metadata.Name
		cat, msg := health.ClassifyIngress(key, len(ing.Status.LoadBalancer.Ingress), phase1)
		record(r, cat, msg)
	}
}

// workflowItem is an Argo Workflow reduced to its phase.
type workflowItem struct {
	meta
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func checkWorkflows(r *health.Report, inv *clusterInventory, phase1 bool) {
	hdr("Argo Workflows (recent Failed / Error)")
	if !inv.crds["workflows.argoproj.io"] {
		return
	}
	for _, wf := range sectionItems[workflowItem](r, "Workflows", "get", "workflows.argoproj.io", "-A") {
		key := wf.Metadata.Namespace + "/" + wf.Metadata.Name
		if cat, msg := health.ClassifyWorkflowPhase(key, wf.Status.Phase, phase1); cat != health.CatOK {
			record(r, cat, msg)
		}
	}
}

func checkStuckFinalizers(r *health.Report, inv *clusterInventory) {
	hdr("stuck-finalizer deletions")
	now := time.Now()
	found := false
	for _, spec := range health.StuckResourceKinds() {
		parts := strings.SplitN(spec, "|", 2)
		kind, scope := parts[0], parts[1]
		if kind != "pv" && kind != "pvc" && !inv.crds[kind] {
			continue
		}
		args := []string{"get"}
		if scope == "-A" {
			args = append(args, "-A")
		}
		args = append(args, kind)
		for _, m := range sectionItems[meta](r, "stuck-finalizer "+kind, args...) {
			if m.Metadata.DeletionTimestamp == "" {
				continue
			}
			del, err := time.Parse(time.RFC3339, m.Metadata.DeletionTimestamp)
			if err != nil {
				continue
			}
			if health.StuckFinalizer(true, len(m.Metadata.Finalizers), now.Sub(del).Seconds()) {
				ns := m.Metadata.Namespace
				if ns == "" {
					ns = "<cluster>"
				}
				record(r, health.CatFail, fmt.Sprintf("%s %s/%s stuck Terminating (finalizers: %s)", kind, ns, m.Metadata.Name, strings.Join(m.Metadata.Finalizers, ",")))
				found = true
			}
		}
	}
	if !found {
		record(r, health.CatOK, "no resources stuck Terminating (>5min with non-empty finalizers)")
	}
}

// podItem is a Pod reduced to the owner references that decide whether it is a
// steady-state workload at all, plus the status the pod predicates judge.
type podItem struct {
	Metadata struct {
		Namespace       string            `json:"namespace"`
		Name            string            `json:"name"`
		OwnerReferences []health.OwnerRef `json:"ownerReferences"`
	} `json:"metadata"`
	Status health.PodStatus `json:"status"`
}

func checkPods(r *health.Report, phase1 bool) {
	hdr("unhealthy pods (all namespaces)")
	bad := false
	for _, p := range sectionItems[podItem](r, "Pods", "get", "pods", "-A") {
		// Job/CronJob pods are ephemeral and self-completing — their health is
		// the Job section's (checkJobs/ClassifyJob), not this steady-state
		// workload gate. Skip them so a short-lived CronJob pod caught
		// mid-creation (e.g. argo-resync-nudger) can't flunk the gate.
		if health.IsJobControlled(p.Metadata.OwnerReferences) {
			continue
		}
		// Ephemeral e2e health-probe pods (Argo-Workflow-owned, so NOT Job-
		// controlled) are test scaffolding — a Failed one from a prior run on a
		// reused cluster must not gate convergence. Same rationale as the Workflow
		// scan (ClassifyWorkflowPhase).
		if health.IsEphemeralE2EProbe(p.Metadata.Name) {
			continue
		}
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		if health.PodIsFailing(p.Status) {
			detail := fmt.Sprintf("Pod %s phase=%s ready=%s state=%s", key, p.Status.Phase, health.ReadyRatio(p.Status), health.SummarizeStates(p.Status))
			switch {
			case phase1 && health.MatchPrefix(key, health.Phase1PendingWorkloads()):
				record(r, health.CatPending, detail+" — waiting on OpenBao bootstrap")
			case extDepMatch(key):
				reason, _ := health.MatchExternalDep(key, health.ExternalDepWorkloads())
				record(r, health.CatDeferred, detail+" — "+reason)
			default:
				record(r, health.CatFail, detail)
			}
			bad = true
		}
		if hot := health.FlappingContainers(p.Status, 5); hot != "" {
			record(r, health.CatWarn, fmt.Sprintf("Pod %s has flapping containers: %s", key, hot))
		}
	}
	if !bad {
		record(r, health.CatOK, "no pods in a failing state")
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

func extDepMatch(key string) bool {
	_, ok := health.MatchExternalDep(key, health.ExternalDepWorkloads())
	return ok
}

// progressingCondition returns a Deployment's Progressing condition reason/message.
func progressingCondition(conds []health.Condition) (reason, message string) {
	for _, c := range conds {
		if c.Type == "Progressing" {
			return c.Reason, c.Message
		}
	}
	return "", ""
}

// countReadyEndpoints sums ready endpoints across a Service's EndpointSlices.
func countReadyEndpoints(ns, svc string) int {
	return health.CountReadyEndpoints(
		kList[health.EndpointSlice]("-n", ns, "get", "endpointslices", "-l", "kubernetes.io/service-name="+svc))
}
