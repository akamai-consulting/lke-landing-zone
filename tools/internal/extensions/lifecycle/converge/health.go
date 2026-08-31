package converge

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
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
// check. The OpenbaoNamespace const eight lines below had the correct name the
// whole time.
//
// Keep the llz- prefixed names in sync with the namespaces the components
// actually create (platform-apl/components/*, kubernetes-charts/*). A rename
// here fails open, so it is worth checking against the tree rather than
// assuming.
// scannedNamespaces is healthNamespaces plus the APP ESTATE — the namespaces
// instance-owned Applications declare into that the platform does not occupy.
//
// WITHOUT THE SECOND HALF THE APP SCOPE CANNOT SEE THE APP ESTATE. These sections
// list per namespace, not -A, so an instance app in a team namespace was examined
// by neither scope: its Deployment, its StatefulSet and — the one nothing else
// catches — its Service's endpoints. A Service whose selector matches nothing is
// reported by no other check, and Argo calls a ClusterIP Service Healthy
// unconditionally, so `--scope=apps` exited 0 over an unreachable app.
//
// InstanceNamespaces, NOT Namespaces. An instance app that declares a single
// ServiceMonitor into monitoring would otherwise pull that whole namespace into
// the per-namespace scan, and apl-core's loki Deployments — platform-owned, never
// scanned here before, and gating — would start deciding the platform verdict on
// the strength of one instance-owned side-car resource. The app estate is what
// this widening is for; the platform's namespaces are already in the list above
// or already judged by the -A sections.
func scannedNamespaces(owned health.OwnershipIndex) []string {
	out := append([]string(nil), healthNamespaces...)
	seen := make(map[string]bool, len(out))
	for _, ns := range out {
		seen[ns] = true
	}
	for _, ns := range owned.InstanceNamespaces() {
		if !seen[ns] {
			out = append(out, ns)
		}
	}
	return out
}

var healthNamespaces = []string{
	"argocd", "kube-system", "cert-manager", "llz-cert-automation", "external-secrets",
	OpenbaoNamespace, "llz-observability", "harbor", "istio-system",
}

const OpenbaoNamespace = "llz-openbao"

// ── converge loop ────────────────────────────────────────────────────────────

// convergeState carries the one piece of poll-to-poll memory a `llz ci converge`
// run keeps: whether phase1 (OpenBao bootstrap pending) has resolved. (An earlier
// per-section memoization was removed — it forced a full confirm-on-DONE pass
// [~a whole extra health scan] on every converge to guard against masking a
// regression, and once the harbor-kick / store-recovery work made converge hit
// color.Green on the FIRST poll, that confirm was pure cost with no multi-poll benefit
// to recoup it. Each poll now just runs the full health scan; the elapsed-aware
// convergeSleep keeps the pacing cheap.)
type convergeState struct {
	// phase1Done: phase1 resolved FALSE once — the platform-app-ca /
	// ClusterSecretStore probes (each up to 3 tries with 3s pauses) never need to
	// run again this converge (leaving phase1 is one-way within a bootstrap).
	phase1Done bool
}

func newConvergeState() *convergeState { return &convergeState{} }

// convergePoll is the health scan the converge loop runs — a seam so the loop's
// CONTROL FLOW (poll → hard-fail re-check → verdict) can be driven directly.
// Reaching a converged (exit 0) verdict through the kubectl fake means satisfying
// every one of the ~20 checks at once, which makes a control-flow test hostage to
// unrelated check changes; the real scan is still exercised end-to-end by the
// fixtures in ci_health_test.go / ci_health_mutation_test.go.
var convergePoll = healthExitCodeState

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

// longPoleCandidates returns the labels keeping the PLATFORM in-progress (Pending
// + Failed). Pure — the tolerated categories (Drift/Deferred/Instance) are
// excluded because they do not hold up platform convergence.
//
// AN EARLIER PASS ADDED Instance HERE, reasoning that an instance-owned Deployment
// pulling an image for fifteen minutes is still the last thing to go healthy. It
// is — for the APP scope. Mixed into the platform list it named content that never
// gated as the cause of a platform timeout, and on the cluster this boundary was
// measured against (37 instance findings) it could push the one item that DID gate
// past the report's 25-line cap. Each scope reports what it was waiting on; see
// appLongPoleCandidates.
func longPoleCandidates(r *health.Report) []string {
	out := make([]string, 0, len(r.Pending)+len(r.Failed))
	out = append(out, r.Pending...)
	out = append(out, r.Failed...)
	return out
}

// appLongPoleCandidates is longPoleCandidates for the app scope: the demoted
// severities, which are exactly what AppVerdict gates on.
func appLongPoleCandidates(r *health.Report) []string {
	out := make([]string, 0, len(r.InstanceFailed)+len(r.InstancePending))
	out = append(out, r.InstanceFailed...)
	out = append(out, r.InstancePending...)
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
	if err := deps.Summary("GITHUB_STEP_SUMMARY", b.String()); err != nil {
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
func runConverge(budget, interval, retryDelay int, scope string) error {
	deadline := time.Now().Add(time.Duration(budget) * time.Second)
	st := newConvergeState()
	// The converge loop itself is the retry for the cluster probes — a transient
	// blip misread on one poll is corrected ~interval later, and a probe that
	// cannot be answered now records CatPending (kubectl_probe.go), which keeps
	// polling rather than resolving. So don't also pay every probe's internal
	// 3×3s retry pauses on every poll. (Restored on return so one-shot
	// `llz ci health` semantics — and tests — keep the retrying probes.)
	prevProbeRetries := kubectlprobe.Retries
	kubectlprobe.Retries = 1
	defer func() { kubectlprobe.Retries = prevProbeRetries }()
	// States a BUDGET will resolve are pending here and terminal in one-shot
	// `llz ci health` — see health.Budgeted. Borrowed and restored exactly like
	// the probe retries above, so scheduled cluster-health and the in-cluster
	// reconciler keep their steady-state verdicts.
	prevBudgeted := health.Budgeted
	health.Budgeted = true
	defer func() { health.Budgeted = prevBudgeted }()
	// Long-pole tracking (Tier-3 instrumentation): remember which apps/resources
	// were still not-OK on the most recent in-progress poll, so on convergence we
	// can report what was the LAST thing to go healthy — confirming the tail's
	// identity across runs instead of assuming it.
	// scoped picks WHICH verdict of the one poll this loop is polling on. It is a
	// closure and not an inline branch because the loop reads a verdict in two
	// places — the poll and the hard-fail re-check — and scoping only the first is
	// not a partial fix but an inverted one: an apps-scope run whose app content
	// hard-failed re-checked the PLATFORM code, found it healthy, and returned
	// success on exactly the state the gate exists to catch.
	scoped := func(res healthResult) int {
		if scope == ScopeApps {
			return res.appCode
		}
		return res.code
	}
	scopedNonOK := func(res healthResult) []string {
		if scope == ScopeApps {
			return res.nonOKApps
		}
		return res.nonOK
	}
	// subject names what this run is judging, so a red apps-scope step does not
	// report that "the cluster hard-failed" for content the cluster does not own.
	subject := "cluster"
	if scope == ScopeApps {
		subject = "apps scope (instance-owned content)"
	}
	var prevNonOK []string
	var prevAttempt int
	redisRealigned := false
	crdAnnotationsStripped := false
	for attempt := 1; ; attempt++ {
		fmt.Fprintf(os.Stderr, "::notice::convergence poll attempt %d\n", attempt)
		pollStart := time.Now()
		res := convergePoll(st)
		step := health.ConvergeStep(scoped(res))
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
		// PLATFORM SCOPE ONLY, both of them. These are repairs — a Deployment
		// restart and a CRD annotation strip — on platform infrastructure. An
		// apps-scope run is gating an app team's content on behalf of an app team;
		// letting it mutate the platform inverts the boundary the scope exists to
		// draw.
		//
		// THE TRADE: a split that first surfaces while only the apps lane is running
		// is observed and not repaired. The apps lane polls its budget and fails
		// (every Application ComparisonErrors under a redis split, instance-owned
		// ones included), and the repair waits for the next platform run. That is
		// the right way round — a lane that cannot fix a fault should report it
		// rather than reach into the platform — but it is a delay, not a no-op.
		if scope == ScopePlatform && res.redisAuthSplit && !redisRealigned {
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
		if scope == ScopePlatform && res.annotationWedge && !crdAnnotationsStripped {
			crdAnnotationsStripped = true
			fmt.Fprintln(os.Stderr, "::warning::an Argo sync hit the 256KB annotation limit — stripping oversized CRD last-applied-configuration annotations")
			deps.StripOversizedCRDLastApplied()
		}
		switch step {
		case health.ConvergeDone:
			// Every poll is a full health scan (no memoized skips), so the DONE
			// verdict already rests on a complete pass — no confirm needed.
			reportConvergeLongPole(prevNonOK, prevAttempt)
			return nil
		case health.ConvergePoll:
			prevNonOK, prevAttempt = scopedNonOK(res), attempt
			if time.Now().After(deadline) {
				// NAME WHAT WAS STILL PENDING. Deferring a verdict to the budget is
				// only honest if the budget's report says what it was waiting for —
				// otherwise the checks that now pend (a pod still being created, a
				// Service whose pods have no IP yet) trade a precise CatFail for a
				// timeout that names nothing, which is a worse answer, not a kinder
				// one. This is the other half of those classifier changes.
				reportConvergePending(scopedNonOK(res))
				fmt.Fprintf(os.Stderr, "::error::budget of %ds exhausted with the %s still in-progress.\n", budget, subject)
				return fmt.Errorf("budget of %ds exhausted with the %s still in-progress", budget, subject)
			}
			time.Sleep(convergeSleep(time.Duration(interval)*time.Second, pollDur))
		case health.ConvergeRetryHard:
			// The re-check is a whole scan (35-58s measured), so the deadline has to
			// bound it like every other branch. It did not, which made `--budget 0`
			// — the report-only snapshot — pay two and sometimes three full scans on
			// exactly the runs where something was broken.
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "::error::%s hard-failed and the %ds budget is exhausted — not re-checking.\n", subject, budget)
				return fmt.Errorf("%s hard-failed with the %ds budget exhausted", subject, budget)
			}
			fmt.Fprintf(os.Stderr, "::warning::hard failure reported — re-checking after %ds to absorb transients.\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			// The re-check is a FULL health scan, so its verdict is worth exactly as
			// much as any poll's — consume it rather than testing only "still hard".
			// Discarding a DONE here cost a whole extra scan (35-58s, measured on
			// every one of 7 sampled release-e2e runs): the hard strike is routinely
			// the Loki pods cycling as they re-render onto S3, which clears within
			// the retry delay, so the re-check said converged and the loop then paid
			// another scan to be told the same thing. Same reasoning that retired the
			// confirm-on-DONE pass (see convergeState).
			recheck := convergePoll(st)
			switch health.ConvergeStep(scoped(recheck)) {
			case health.ConvergeRetryHard:
				fmt.Fprintf(os.Stderr, "::error::%s hard-failed twice in a row — operator intervention required.\n", subject)
				return fmt.Errorf("%s hard-failed twice in a row — operator intervention required", subject)
			case health.ConvergeDone:
				reportConvergeLongPole(prevNonOK, prevAttempt)
				return nil
			}
			// recovered to in-progress (or the apiserver blipped) — keep polling
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
	if out, err := deps.W().RolloutRestart("argocd", "deploy/argocd-redis"); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::argocd-redis rollout restart failed (%v): %s\n", err, strings.TrimSpace(string(out)))
		return
	}
	if _, err := deps.Exec("kubectl", "-n", "argocd", "rollout", "status", "deploy/argocd-redis", "--timeout=120s"); err != nil {
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
	// nonOK is the scan's Pending+Failed labels — the platform convergence long
	// pole. nonOKApps is the same measurement for the app scope: the demoted
	// severities AppVerdict gates on. Carried separately so a run reports the items
	// ITS scope was waiting on, rather than leading a red app step with platform
	// findings it does not gate.
	nonOK     []string
	nonOKApps []string
	// redisAuthSplit: a repo-server↔argocd-redis auth split (WRONGPASS/NOAUTH) was
	// seen; runConverge self-heals by restarting argocd-redis once.
	redisAuthSplit bool
	// annotationWedge: an Argo app sync failed on the 256KB metadata.annotations
	// limit; runConverge self-heals by stripping the oversized CRD annotation once.
	annotationWedge bool
	// appCode is the SAME report judged over its instance-owned half — the
	// content the platform contract deliberately does not gate on. Carried
	// alongside rather than instead of `code` so one scan answers both scopes and
	// the two can never disagree about what they saw.
	appCode int
}

// ScopePlatform / ScopeApps name the two halves of one report. The platform scope
// is the convergence contract as it has always been; the apps scope gates the
// instance-owned content the boundary excludes from it.
//
// SEPARATING THEM IS NOT DROPPING ONE. An instance's apps stop blocking a
// platform release, and in exchange they get a gate of their own — run as its own
// step, with its own owner. Without the second half, "does not gate the platform"
// quietly means "nothing goes red", which is how eight unseeded per-app
// credentials survived eight days on akamai/gsap-apl.
const (
	ScopePlatform = "platform"
	ScopeApps     = "apps"
)

// healthExitCodeFor runs the checks once and returns the exit code for one scope.
func healthExitCodeFor(scope string) int {
	res := healthExitCodeState(nil)
	if scope == ScopeApps {
		return res.appCode
	}
	return res.code
}

// bothScopes is the exit code for a state neither scope could look past — an
// unreachable apiserver, a cluster that has not bootstrapped, a namespace list
// that failed. BOTH codes are set because healthResult.appCode's zero value is 0,
// which means Converged: an early return that fills in only `code` reports the app
// scope green for a cluster it never read, and that is indistinguishable from the
// outage it exists to catch.
func bothScopes(code int) healthResult { return healthResult{code: code, appCode: code} }

// healthExitCodeState is healthExitCode with optional converge state: nil for a
// one-shot `llz ci health`, non-nil inside `llz ci converge`, where the only
// carried fact is phase1Done (so the phase1 probes resolve once per run, not per
// poll). Every poll runs the full set of checks below.
func healthExitCodeState(st *convergeState) healthResult {
	if !kubectlprobe.Reachable() {
		// Exit 3 (not 1): an unreachable apiserver is an infrastructure transient,
		// not a cluster hard-failure. The converge loop retries it against the
		// budget instead of counting it as a hard strike (see runConverge).
		fmt.Fprintln(os.Stderr, "::error::kubectl cannot reach the apiserver — check KUBECONFIG and cluster reachability.")
		return bothScopes(3)
	}

	inv := scanCRDs()

	// Phase 0: pre-bootstrap (Argo CRD / platform-bootstrap App not present yet)
	// is in-progress, not converged — poll. Gated on the CRD list alone, before
	// the namespace fetch: a pre-bootstrap cluster is polled every interval for
	// the whole apl-core helmfile run, and it has nothing for the per-namespace
	// sections to look at yet.
	if !inv.crds["applications.argoproj.io"] ||
		!kubectlprobe.Exists("-n", "argocd", "get", "application", "platform-bootstrap") {
		fmt.Println(color.Bold("== pre-bootstrap phase detected — apl-core helmfile likely still running =="))
		fmt.Printf("  %s applications.argoproj.io CRD or platform-bootstrap Application not yet present\n", color.Cyan("PENDING"))
		return bothScopes(2)
	}

	if !inv.addNamespaces() {
		// Exit 3, same as an unreachable apiserver: kubectlprobe.Reachable() has already
		// passed, so a failed namespace list is a transient, not a verdict. Reading
		// it as "no namespaces exist" would skip every per-namespace section and
		// report a broken cluster as converged.
		fmt.Fprintln(os.Stderr, "::error::kubectl could not list namespaces — treating as an apiserver transient, not an empty cluster.")
		return bothScopes(3)
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
	checkLokiObjStorage(&r, phase1)
	checkFirewallBootstrap(&r)
	checkOpenBao(&r, phase1)
	argoApps, argoOK := fetchArgoApps()
	owned := health.NewOwnershipIndex(argoApps).WithPlatformNamespaces(healthNamespaces)
	checkReadyResources(&r, owned, phase1)
	checkWebhooks(&r)
	checkAppProjects(&r, inv)
	checkLeases(&r, inv)
	checkArgoApps(&r, argoApps, argoOK, phase1)
	checkWorkloads(&r, inv, owned, phase1)
	checkPVCs(&r, owned)
	checkPVs(&r)
	checkNetworkPolicies(&r, inv)
	checkJobs(&r, owned, phase1)
	checkCronWorkflows(&r, inv, owned)
	checkServices(&r, inv, owned, phase1)
	checkPDBs(&r, owned, phase1)
	checkIngresses(&r, owned, phase1)
	checkWorkflows(&r, inv, owned, phase1)
	checkStuckFinalizers(&r, inv, owned)
	checkPods(&r, owned, phase1)

	printHealthSummary(&r, owned)

	// In phase1 the support plane is still installing (apl-core's CRDs, webhook
	// Services, and endpoints land in later helmfile phases), so a hard-fail here
	// is "not yet installed", not terminal — downgrade it to in-progress so
	// converge keeps polling until the cluster advances past phase1 instead of
	// aborting on still-installing infra. See health.PhaseAwareExitCode.
	//
	// The downgrade is vetoed by a git-auth failure. Its premise — "not yet
	// installed" — does not hold for a credential the remote has already rejected:
	// no later helmfile phase mints one, so every extra poll is dead time. A converge
	// run has burned its whole 1200s budget exactly this way, then reported "budget
	// exhausted with the cluster still in-progress" for a cluster that was not
	// progressing at all.
	demotePhase1 := phase1 && !r.GitAuthFailure
	code := health.PhaseAwareExitCode(r.ExitCode(), demotePhase1)
	switch {
	case demotePhase1 && code != r.ExitCode():
		fmt.Println(color.Bold("== phase1 (support plane still installing) — hard failures above are treated as in-progress; converge will keep polling =="))
	case phase1 && r.GitAuthFailure:
		fmt.Println(color.Bold("== phase1, but NOT downgrading: Argo CD is being refused by the git remote — polling cannot fix a rejected credential =="))
		fmt.Fprintln(os.Stderr, "::error::Argo CD cannot authenticate to the source repo. This is terminal — check the values-repo credential (APL_VALUES_REPO_TOKEN → otomi.git.password → the argocd repo Secret) rather than re-running.")
	}
	return healthResult{
		code: code,
		// NOT PhaseAwareExitCode. phase1's premise is "apl-core's support plane is
		// still installing", and apl-core installs no instance-owned content — an
		// app's missing credential is no less terminal for being early. Its own
		// still-settling states already classify as Pending, which is exit 2 here.
		appCode: r.AppVerdict().ExitCode(),
		// The still-converging set for the converge long-pole report (Tier-3
		// instrumentation): Pending + Failed are the categories that keep the
		// cluster in-progress (Drift/Deferred are tolerated-as-converged), so they
		// are the candidates for "last thing to go healthy".
		nonOK:           longPoleCandidates(&r),
		nonOKApps:       appLongPoleCandidates(&r),
		redisAuthSplit:  r.RedisAuthSplit,
		annotationWedge: r.AnnotationLimitWedge,
	}
}

func printHealthSummary(r *health.Report, owned health.OwnershipIndex) {
	fmt.Println()
	for _, c := range r.Drift {
		fmt.Println("  " + color.Yellow("drift:   ") + " " + c)
	}
	for _, c := range r.Deferred {
		fmt.Println("  " + color.Cyan("deferred:") + " " + c)
	}
	for _, c := range r.Pending {
		fmt.Println("  " + color.Cyan("pending: ") + " " + c)
	}
	for _, c := range r.Instance {
		fmt.Println("  " + color.Magenta("instance:") + " " + c)
	}
	for _, c := range r.Failed {
		fmt.Println("  " + color.Red("FAILED:  ") + " " + c)
	}
	// One dead tunnel fails every apiserver→pod check at once. Name it as the single
	// cause it is — otherwise the reader sees N unrelated component failures sitting
	// directly under a color.Green "konnectivity-agent (3/3)" line and debugs the symptoms.
	if r.TunnelDown {
		fmt.Println(color.Yellow("  konnectivity tunnel (apiserver → pod) unavailable — the checks above that " +
			"depend on it are inconclusive, not failed. konnectivity-agent reporting Ready does not " +
			"prove the tunnel: its readiness probe does not exercise the dial-out."))
	}
	// THE APP SCOPE IS NAMED EVEN WHEN THE PLATFORM PASSES, because the whole risk
	// of this boundary is that excluded content becomes invisible. A reader of a
	// green platform report must still be told, in the summary and not only in the
	// INSTANCE lines above, that app-owned content is broken and which lane owns it.
	//
	// AND IT IS AN ANNOTATION, not just a printed line. A green job's log is not
	// read: the whole failure this boundary exists to fix was eight credentials
	// nobody looked at for eight days. ::warning:: surfaces on the job summary
	// where a passing platform run is still visibly carrying broken app content.
	if n := len(r.InstanceFailed); n > 0 {
		msg := fmt.Sprintf("%d instance-owned check(s) hard-failed — reported here, gated by `llz ci converge --scope=apps`, NOT by the platform contract.", n)
		fmt.Printf("%s %s\n", color.Magenta("!"), msg)
		fmt.Fprintf(os.Stderr, "::warning::%s\n", msg)
	} else if n := len(r.InstancePending); n > 0 {
		msg := fmt.Sprintf("%d instance-owned check(s) still converging — gated by `llz ci converge --scope=apps`.", n)
		fmt.Printf("%s %s\n", color.Magenta("!"), msg)
		fmt.Fprintf(os.Stderr, "::warning::%s\n", msg)
	}
	// The index answers from Argo's .status.resources, and there are exactly two
	// states where that answer is incomplete. Both are reported rather than
	// silently absorbed: a reader who sees the platform gate on app content needs
	// to know the boundary could not resolve it, not conclude the boundary is off.
	if n := owned.Contested(); n > 0 {
		fmt.Printf("  %s %s\n", color.Yellow("boundary:"), fmt.Sprintf("%d resource(s) an instance-owned Application declares are ALSO declared by a platform Application — kept gating the platform.", n))
	}
	// "ARGO HAS NOT COMPARED THEM" IS THE CONDITION, not "declares no resources",
	// and the line says so because the difference is what makes it transient. A
	// platform Application Argo HAS compared and that owns nothing never reaches
	// this list — apl-core's global/team gitops shells are Synced with an empty
	// .status.resources permanently, and while zero resources alone put an app
	// here the veto could never lift on an instance that runs them.
	//
	// THE SCOPE IS STATED, NOT GUESSED. The previous wording ("nothing in a
	// platform namespace is demotable") was wrong in both directions: a veto
	// bounded to one destination leaves the OTHER platform namespaces demotable on
	// that same poll, and — the half nobody could see — Owns switches the
	// app-estate inference off EVERYWHERE while this list is non-empty, so a
	// team-namespace failure was gating for a reason the report never printed.
	if u := owned.PlatformUnresolved(); len(u) > 0 {
		scope := "every platform namespace (one of them names no destination, so nothing bounds it)"
		if !owned.PlatformUnresolvedAnywhere() {
			scope = "their destination namespace(s): " + strings.Join(owned.PlatformUnresolvedNamespaces(), ", ")
		}
		msg := fmt.Sprintf("%d platform Application(s) have not been compared by Argo yet (%s) — the boundary cannot tell what they own. Demotion is off in %s; and anywhere in the app estate, a resource no Application declares stays platform until they resolve (direct claims still demote). Transient: it clears when Argo finishes comparing them.",
			len(u), strings.Join(u, ", "), scope)
		fmt.Printf("  %s %s\n", color.Yellow("boundary:"), msg)
	}
	// NAMED BEFORE THE OTHER BOUNDARY LINES, because it is the only one an
	// operator can act on in a minute. Every other state here is the boundary
	// working; this one is the boundary being bypassed by a missing field.
	if m := owned.Misprojected(); len(m) > 0 {
		msg := fmt.Sprintf("%d Application(s) deploy only into the app estate but are NOT in the `%s` AppProject (%s) — so they and everything they declare GATE THE PLATFORM. Set `spec.project: %s` to move them to the apps scope.",
			len(m), health.InstanceCustomProject, strings.Join(m, ", "), health.InstanceCustomProject)
		fmt.Printf("  %s %s\n", color.Yellow("boundary:"), msg)
		fmt.Fprintf(os.Stderr, "::warning::%s\n", msg)
	}
	if ns := owned.InstanceNamespaces(); len(ns) > 0 {
		fmt.Printf("  %s %s\n", color.Yellow("boundary:"), fmt.Sprintf("app estate scanned for the apps scope: %s — a resource in one of these that no Application declares is treated as instance-owned.", strings.Join(ns, ", ")))
	}
	if u := owned.Unresolved(); len(u) > 0 {
		fmt.Printf("  %s %s\n", color.Yellow("boundary:"), fmt.Sprintf("%d instance-owned Application(s) declare no resources yet (%s) — Argo has not compared them, so THEIR content still gates the platform on this poll.", len(u), strings.Join(u, ", ")))
	}
	switch r.Verdict() {
	case health.HardFailed:
		fmt.Printf("%s\n", color.Red(fmt.Sprintf("%d check(s) hard-failed.", len(r.Failed))))
	case health.InProgress:
		fmt.Println(color.Yellow("Cluster is still converging — re-run after a backoff."))
	default:
		switch {
		case len(r.Deferred) > 0 && len(r.Instance) > 0:
			fmt.Printf("%s %s\n", color.Green("✓"), fmt.Sprintf("Platform converged — %d operator-deferred + %d instance-owned item(s) remain (neither gates the platform).", len(r.Deferred), len(r.Instance)))
		case len(r.Instance) > 0:
			fmt.Printf("%s %s\n", color.Green("✓"), fmt.Sprintf("Platform converged — %d instance-owned item(s) remain (operator-owned escape hatch; does not gate the platform).", len(r.Instance)))
		case len(r.Deferred) > 0:
			fmt.Printf("%s %s\n", color.Green("✓"), fmt.Sprintf("Cluster converged — %d operator-deferred item(s) remain, platform healthy.", len(r.Deferred)))
		default:
			fmt.Printf("%s Cluster converged.\n", color.Green("✓"))
		}
	}

	// Both verdicts, every time. The scope flag decides the exit code, not what the
	// reader is told: a run that exits 1 on the app scope must not end with a green
	// platform line as its last word.
	if len(r.Instance) > 0 || r.AppVerdict() != health.Converged {
		switch r.AppVerdict() {
		case health.HardFailed:
			fmt.Println(color.Magenta(fmt.Sprintf("apps scope: %d instance-owned check(s) hard-failed.", len(r.InstanceFailed))))
		case health.InProgress:
			if r.Inconclusive && len(r.InstancePending) == 0 {
				fmt.Println(color.Magenta("apps scope: INCONCLUSIVE — a corpus could not be read, so the app estate was not fully examined."))
				break
			}
			fmt.Println(color.Magenta("apps scope: instance-owned content still converging."))
		default:
			fmt.Println(color.Magenta("apps scope: converged (instance-owned findings are informational only)."))
		}
	}
}

// ── kubectl helpers ──────────────────────────────────────────────────────────

func phase1OpenBaoBootstrapPending() bool {
	// kExists retries an unanswerable probe (kubectl_probe.go), so a transient
	// API/ACL blip no longer reads as a missing CA and mislabels the phase.
	if kubectlprobe.Exists("-n", "cert-manager", "get", "secret", "platform-app-ca") {
		return false
	}
	return !openBaoClusterSecretStoreReadyWithRetry()
}

func openBaoClusterSecretStoreReadyWithRetry() bool {
	for attempt := 0; attempt < kubectlprobe.Retries; attempt++ {
		if openBaoClusterSecretStoreReady() {
			return true
		}
		if attempt < kubectlprobe.Retries-1 {
			time.Sleep(kubectlprobe.Delay)
		}
	}
	return false
}

func openBaoClusterSecretStoreReady() bool {
	out, err := deps.Exec("kubectl", "get", "clustersecretstore", defaultSecretStore, "-o", "json")
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
// empty list and report color.Green. This is requireCorpus for cluster probes: a
// section that had nothing to check otherwise prints the same clean run as one
// that checked everything. CatPending (not CatFail) because converge's poll loop
// should re-ask — an unreadable cluster is not converged, but it is also not
// proof of a broken one; the budget decides.
func sectionItems[T any](r *health.Report, kind string, args ...string) []T {
	items, ok := kubectlprobe.ListOK[T](args...)
	if !ok {
		printFinding(r.RouteInconclusive(kind))
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
// than banking a false color.Green. The CRD list needs no such handling — a failed one
// empties inv.crds, which trips the phase-0 gate into exit 2 and short-circuits
// before any CRD-driven section runs.
// scanCRDs takes the first of the two list calls. A failed one empties inv.crds,
// which trips the phase-0 gate into exit 2 — loud enough, and the same posture
// the per-name CRD probes had, since that same CRD gated phase-0 by name before.
func scanCRDs() *clusterInventory {
	inv := &clusterInventory{crds: map[string]bool{}, nsExists: map[string]bool{}}
	for _, crd := range kubectlprobe.List[meta]("get", "crd") {
		inv.crds[crd.Metadata.Name] = true
	}
	return inv
}

// addNamespaces takes the second, and reports whether the call actually
// succeeded. Called after the phase-0 gate so a pre-bootstrap poll — which has
// no namespaces worth listing — does not pay for it every interval.
func (inv *clusterInventory) addNamespaces() bool {
	raw, ok := kubectlprobe.ItemsOK("get", "ns")
	if !ok {
		return false
	}
	inv.namespaces = kubectlprobe.DecodeItems[namespaceItem](raw)
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
	paint func(string) string
}{
	health.CatOK:       {"OK", color.Green},
	health.CatWarn:     {"WARN", color.Yellow},
	health.CatFail:     {"FAIL", color.Red},
	health.CatPending:  {"PENDING", color.Cyan},
	health.CatDeferred: {"DEFERRED", color.Cyan},
	health.CatDrift:    {"DRIFT", color.Yellow},
	health.CatInstance: {"INSTANCE", color.Magenta},
}

// record, recordRes, recordPod and recordApp are PRINTERS. Every rule about what
// a finding means — the konnectivity downgrade, the platform/instance boundary,
// which bucket it lands in — lives in health/routing.go, which each of these
// hands the finding to and then prints whatever category comes back.
//
// It used to be the other way round: recordRes/recordPod decided ownership here
// and returned before reaching record(), which is where the konnectivity
// downgrade lives — so a tunnel outage on an instance-owned resource was banked
// as a hard failure for the app lane and never raised TunnelDown. Keeping the
// decisions in one package and the printing in another is what stops the next
// invariant from having to be remembered at three call sites.

// record is for a cluster-wide fact — a node, a webhook, a lease — with no
// resource identity to attribute to an owner.
func record(r *health.Report, cat health.Category, msg string) {
	printFinding(r.Route(cat, msg))
}

// recordRes is for a finding that names one identifiable resource. Every check
// that judges a resource calls this, so the ownership boundary cannot be applied
// in some sections and forgotten in others.
func recordRes(r *health.Report, owned health.OwnershipIndex, cat health.Category, ref health.ResourceRef, msg string) {
	printFinding(owned.Route(r, cat, ref, msg))
}

// recordOwned is for a resource a controller generated, which therefore appears
// in no Application's declared set under that name: a Pod, a Workflow spawned by a
// CronWorkflow, a CertificateRequest cert-manager made for a Certificate, a Job a
// CronJob created. Ownership resolves through the controller that IS declared.
func recordOwned(r *health.Report, owned health.OwnershipIndex, cat health.Category, owners []health.OwnerRef, self health.ResourceRef, msg string) {
	printFinding(owned.RouteOwned(r, cat, owners, self, msg))
}

// recordApp is for one Argo Application's own finding — the Application-level half
// of the boundary, which records the demoted severity so the app scope gates on it.
func recordApp(r *health.Report, a health.ArgoApp, phase1 bool) {
	printFinding(r.RouteApp(a, phase1))
}

// printFinding renders one categorized line. Split out of record() so the
// ownership-aware funnels above print identically instead of re-rolling the
// column/paint logic.
func printFinding(cat health.Category, msg string) {
	style := catStyles[cat]
	// Pad to the fixed column on the PLAIN label, then color — the ANSI escapes are
	// zero-width, so the columns stay aligned (color.go).
	label := fmt.Sprintf("%-8s", style.label)
	if style.paint != nil {
		label = style.paint(label)
	}
	fmt.Printf("  %s %s\n", label, msg)
}

func hdr(s string) { fmt.Printf("\n%s\n", color.Bold("== "+s+" ==")) }

// metaName / nsName extract common metadata for inline-typed items.
type meta struct {
	Metadata struct {
		Namespace         string            `json:"namespace"`
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Finalizers        []string          `json:"finalizers"`
		// OwnerReferences is read by the ownership boundary: a controller-generated
		// resource (a CertificateRequest, a CronWorkflow's Workflow, a CronJob's Job)
		// appears in no Application's declared set under its generated name, so it is
		// only ever reachable through the controller that IS declared.
		OwnerReferences []health.OwnerRef `json:"ownerReferences"`
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
	// CRDs from OPTIONAL/ManagedSkip components (argo-workflows / argo-events) are
	// required ONLY when their owning Application is deployed. On managed (argo skipped)
	// or a self-install that never opted in, the app is absent → the CRD is not expected.
	for crd, app := range health.ConditionalCRDs() {
		switch {
		case inv.crds[crd]:
			record(r, health.CatOK, "CRD "+crd+" installed")
		case kubectlprobe.Exists("-n", "argocd", "get", "application", app):
			record(r, health.CatFail, "CRD "+crd+" missing — owning ArgoCD Application "+app+" has not installed it")
		default:
			record(r, health.CatOK, "CRD "+crd+" not required ("+app+" Application not deployed)")
		}
	}
}

func checkStorageClasses(r *health.Report) {
	hdr("StorageClasses")
	for _, sc := range health.RequiredStorageClasses() {
		if kubectlprobe.Exists("get", "storageclass", sc) {
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

	// Audit EVERY linodebs StorageClass's CSI parameters. The driver silently
	// ignores misspelled keys, so a class that LOOKS encrypted+tagged can be
	// neither; and a PVC born on any linodebs class lacking the lke<id> tag yields
	// a Volume reap can't attribute. Full audit on block-storage-retain, plus a
	// coverage check on every other linodebs class.
	for _, f := range health.AuditLinodeStorageClasses(classes) {
		record(r, f.Cat, "  "+f.Msg)
	}
}

// checkLokiObjStorage gates convergence on the apl-overlay obj chain. On managed
// apl-core the llz-reconciler pushes the AplObjectStorage CR onto apl-<env>,
// apl-operator applies it, and Loki re-renders onto S3 — a reconciler→apl-operator→
// restart chain that is EVENTUAL and legitimately slower than a one-shot check, so
// Loki-on-S3 belongs in the convergence contract (poll), NOT in a post-converge
// assertion that races it (the very flake this fixes: assert-loki checked before the
// chain settled). The in-progress message doubles as the diagnostic — it names WHERE
// the chain is, so a genuine stall is self-explaining on budget exhaustion.
//
// Only gates when obj is actually configured for THIS deployment: LLZ materializes
// apl-secrets/obj-secrets from OpenBao iff the obj credential is seeded. Absent →
// filesystem Loki is intentional (no objectStorage.cluster) → nothing to await.
// Skipped in phase1 (Loki/apl-secrets not installed yet).
func checkLokiObjStorage(r *health.Report, phase1 bool) {
	if phase1 {
		return
	}
	// ExistsOK, because a silent `return` is the strongest possible pass: the
	// section records nothing at all, and Report.Verdict() is default-Converged.
	// With bare Exists an unreadable apiserver skipped the whole check, so the
	// question "is Loki still filesystem-backed?" was never asked and converge
	// exited 0. Absence is a legitimate skip; not knowing is not.
	objSeeded, answered := kubectlprobe.ExistsOK("get", "secret", "obj-secrets", "-n", "apl-secrets")
	if !answered {
		hdr("apl-overlay obj storage (Loki S3)")
		cat, msg := health.PendingIfBudgeted(
			"apl-overlay: could not read apl-secrets/obj-secrets, so whether this deployment uses obj storage is unknown — retrying against the budget",
			"apl-overlay: apl-secrets/obj-secrets could not be read after retries, so this section rendered no verdict on whether Loki is S3-backed")
		record(r, cat, msg)
		return
	}
	if !objSeeded {
		return
	}
	hdr("apl-overlay obj storage (Loki S3)")
	// LokiConfigTextOK, because "" has two meanings and only one of them is a pass.
	// LokiConfigText concatenates every matching ConfigMap's data, and its source —
	// kubectlprobe.Items — returns nil on ANY error. So an unreadable
	// `get configmap -A` produced "" and was graded "Loki not deployed", recording
	// CatOK and letting converge exit 0 with Loki still filesystem-backed. The one
	// state this section exists to catch was reported as the state where it does not
	// apply.
	cfg, listed := health.LokiConfigTextOK("loki")
	if !listed {
		cat, msg := health.PendingIfBudgeted(
			"apl-overlay: ConfigMaps unreadable — cannot yet tell whether Loki is deployed; retrying against the budget",
			"apl-overlay: ConfigMaps could not be read after retries, so whether Loki is S3-backed is UNKNOWN — this is not evidence that Loki is undeployed")
		record(r, cat, msg)
		return
	}
	if strings.TrimSpace(cfg) == "" {
		record(r, health.CatOK, "Loki not deployed — no obj overlay to await")
		return
	}
	if health.LokiConfigUsesS3(cfg) {
		record(r, health.CatOK, "Loki config references S3 — apl-overlay obj converged")
		return
	}
	// Not S3 yet — POLL (CatPending), and report which chain stage is outstanding.
	stage := "reconciler push / apl-operator apply pending — loki-s3-linode-credentials not built yet"
	if kubectlprobe.Exists("get", "secret", "loki-s3-linode-credentials", "-n", "monitoring") {
		stage = "creds built (loki-s3-linode-credentials present) — Loki re-rendering/restarting onto S3"
	}
	record(r, health.CatPending, "apl-overlay: Loki not yet S3-backed — "+stage+" (obj chain settling; check llz-reconciler llz_apl_overlay_synced)")
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
	depExists, depAnswered := kubectlprobe.ExistsOK("-n", "kube-system", "get", "deployment", deps.FirewallDeploymentName)
	cmExists, cmAnswered := kubectlprobe.ExistsOK("-n", "kube-system", "get", "configmap", deps.FirewallConfigMapName)
	if !depAnswered || !cmAnswered {
		record(r, health.CatPending, "could not read kube-system firewall-controller Deployment/ConfigMap — cannot tell 'component disabled' from 'unreadable cluster'")
		return
	}
	if !depExists && !cmExists {
		record(r, health.CatOK, "firewall-controller not installed (cidrFirewall component disabled) — skipped")
		return
	}
	// The kube-system/linode Secret is the CONTROLLER's token — mounted as env
	// LINODE_TOKEN by the llz-linode-cidr-firewall Deployment. The in-cluster
	// self-discovery (reconciler --reconcile-cidr-firewall, which retired the
	// `bootstrap-cloud-firewall` CI seed) authenticates with its OWN ESO-synced
	// linode-api-token and writes the ConfigMap WITHOUT ever seeding this Secret.
	// So the token is required only where the controller Deployment is actually
	// present (the private chart). On instances where the self-discovery ran but
	// the controller is absent (public adopters / e2e — private chart unavailable)
	// the ConfigMap exists yet the token is consumed by nothing: gate the
	// assertion on depExists, not cmExists, so a self-discovery-only cluster does
	// not hard-fail on a Secret no workload reads.
	if depExists {
		exists := kubectlprobe.Exists("-n", "kube-system", "get", "secret", "linode")
		token := ""
		if exists {
			token = kubectlprobe.JSONPath("-n", "kube-system", "get", "secret", "linode", "-o", "jsonpath={.data.token}")
		}
		cat, msg := health.ClassifyFirewallToken(exists, token)
		record(r, cat, msg)
	} else {
		record(r, health.CatOK, "firewall-controller Deployment absent — self-discovery ConfigMap only; kube-system/linode token not required")
	}

	// firewallConfigMapName (ci_firewall.go) is the single source of truth for the
	// ConfigMap name the private chart renders (<fullname>-config =
	// llz-linode-cidr-firewall-config) and `bootstrap-cloud-firewall` patches.
	if !kubectlprobe.Exists("-n", "kube-system", "get", "configmap", deps.FirewallConfigMapName) {
		record(r, health.CatFail, "ConfigMap kube-system/"+deps.FirewallConfigMapName+" missing")
		return
	}
	record(r, health.CatOK, "ConfigMap kube-system/"+deps.FirewallConfigMapName+" exists")
	for _, key := range []string{"LINODE_FIREWALL_ID", "LKE_CLUSTER_ID", "FIREWALL_TEMPLATE_ID", "RECONCILE_INTERVAL_SECS", "VPC_CIDR"} {
		val := kubectlprobe.JSONPath("-n", "kube-system", "get", "configmap", deps.FirewallConfigMapName, "-o", "jsonpath={.data."+key+"}")
		cat := health.ClassifyFirewallConfigKey(key, val)
		if cat == health.CatOK {
			record(r, health.CatOK, "  "+key+" = "+val)
		} else {
			record(r, health.CatDeferred, "  "+key+" empty (set when the firewall bootstrap / Argo app runs)")
		}
	}
}

func checkReadyResources(r *health.Report, owned health.OwnershipIndex, phase1 bool) {
	// cert-manager ClusterIssuers / Certificates / CertificateRequests + ESO.
	readyKind(r, owned, "cert-manager.io", "ClusterIssuer", []string{"get", "clusterissuers.cert-manager.io"}, false,
		func(key string) bool { return phase1 && health.MatchPrefix(key, health.Phase1PendingIssuers()) },
		health.ExternalDepIssuers())
	readyKind(r, owned, "cert-manager.io", "Certificate", []string{"get", "certificates.cert-manager.io", "-A"}, true,
		func(key string) bool { return phase1 && health.MatchPrefix(key, health.Phase1PendingCerts()) },
		health.ExternalDepCerts())
	certRequests(r, owned, phase1)
	readyKind(r, owned, "external-secrets.io", "ClusterSecretStore", []string{"get", "clustersecretstores.external-secrets.io"}, false,
		func(string) bool { return phase1 }, nil)
	readyKind(r, owned, "external-secrets.io", "ExternalSecret", []string{"get", "externalsecrets.external-secrets.io", "-A"}, true,
		func(string) bool { return phase1 }, health.ExternalDepExternalSecrets())
}

// readyResourceItem is a resource with a Ready condition.
type readyResourceItem struct {
	meta
	Status struct {
		Conditions []health.Condition `json:"conditions"`
	} `json:"status"`
}

func readyKind(r *health.Report, owned health.OwnershipIndex, group, kind string, getArgs []string, namespaced bool, phase1Pending func(key string) bool, extDep []health.DepEntry) {
	hdr(kind + "s")
	for _, it := range sectionItems[readyResourceItem](r, kind+"s", getArgs...) {
		key := it.Metadata.Name
		if namespaced {
			key = it.Metadata.Namespace + "/" + it.Metadata.Name
		}
		status, reason, msg := health.FindReady(it.Status.Conditions)
		cat, line := health.ClassifyReady(kind, key, status, reason, msg, phase1Pending(key), extDep)
		recordRes(r, owned, cat, health.ResourceRef{Group: group, Kind: kind, Namespace: it.Metadata.Namespace, Name: it.Metadata.Name}, line)
	}
}

func certRequests(r *health.Report, owned health.OwnershipIndex, phase1 bool) {
	hdr("CertificateRequests")
	for _, it := range sectionItems[readyResourceItem](r, "CertificateRequests", "get", "certificaterequests.cert-manager.io", "-A") {
		key := it.Metadata.Namespace + "/" + it.Metadata.Name
		status, reason, msg := health.FindReady(it.Status.Conditions)
		p1 := phase1 && health.MatchPrefix(key, health.Phase1PendingCerts())
		cat, line := health.ClassifyCertificateRequest(key, status, reason, msg, p1, health.ExternalDepCerts())
		// cert-manager generates the CertificateRequest, so no Application declares
		// it — but the Certificate above it is declared, and demoting one without the
		// other reported a single logical failure on both sides of the boundary.
		recordOwned(r, owned, cat, it.Metadata.OwnerReferences,
			health.ResourceRef{Group: "cert-manager.io", Kind: "CertificateRequest", Namespace: it.Metadata.Namespace, Name: it.Metadata.Name}, line)
	}
}

func checkOpenBao(r *health.Report, phase1 bool) {
	hdr("openbao seal / HA")
	// A CatWarn skip never affects the verdict, so an unreadable STS would retire
	// the entire seal check silently — demand an actual answer before skipping.
	specReplicas, answered := kubectlprobe.JSONPathOK("-n", OpenbaoNamespace, "get", "sts", "platform-openbao", "-o", "jsonpath={.spec.replicas}")
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
		if !kubectlprobe.Exists("-n", OpenbaoNamespace, "get", "pod", pod) {
			record(r, health.CatFail, "Pod openbao/"+pod+" missing")
			continue
		}
		ready := kubectlprobe.JSONPath("-n", OpenbaoNamespace, "get", "pod", pod, "-o", `jsonpath={.status.containerStatuses[?(@.name=="openbao")].ready}`)
		if ready != "true" {
			record(r, health.CatPending, "Pod openbao/"+pod+" (openbao container not Ready — can't query seal status)")
			continue
		}
		// Loopback listener + CA verification (baoLoopbackEnv): the network
		// listener requires a client certificate, which an exec'd `bao` has not got.
		execArgv := append([]string{"-n", OpenbaoNamespace, "exec", pod, "-c", "openbao", "--", "env"}, deps.BaoLoopbackEnv()...)
		execArgv = append(execArgv, "bao", "status", "-format=json")
		out, execErr := deps.Exec("kubectl", execArgv...)
		st, perr := health.ParseBaoStatus(out)
		if perr != nil {
			// `bao status` runs through `kubectl exec`, i.e. the konnectivity tunnel.
			// The exec error was previously discarded, so a dead tunnel surfaced as the
			// unattributable "could not parse bao status JSON" on all three pods —
			// reading as an OpenBao fault when OpenBao was never reached. Carry the
			// stderr into the message: it names the real cause, and lets record()
			// classify a tunnel outage as Pending rather than a hard failure.
			msg := "Pod openbao/" + pod + " (could not parse bao status JSON"
			if detail := strings.TrimSpace(kubectlprobe.ErrText(execErr)); detail != "" {
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
			if kubectlprobe.Exists("-n", OpenbaoNamespace, "exec", pod, "-c", "openbao", "--", "test", "-s", "/openbao/audit/audit.log") {
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
				exists := kubectlprobe.Exists("-n", ns, "get", "svc", svc)
				ready, _ := endpointCounts(ns, svc)
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
		if kubectlprobe.Exists("-n", "argocd", "get", "appproject", ap) {
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
	// healthNamespaces, NOT a second hand-written list — and the second list is
	// exactly what was here. It still said "cert-automation" and "openbao", the
	// PRE-llz-rename names, so `if !inv.nsExists[ns] { continue }` skipped both
	// silently and `stale` stayed false, and this section recorded
	// "all controller Leases renewed" having never looked at the OpenBao or
	// cert-automation leader Leases at all.
	//
	// That is the regression healthNamespaces' own header records, one function
	// over: the rename was fixed in the list above and not in the copy down here,
	// because nothing joined them. Iterating the shared list is the fix — correcting
	// two strings would leave the next rename free to do it again.
	//
	// IT WIDENS THE CHECK, and that is deliberate and worth stating: the shared list
	// adds llz-observability, harbor and istio-system. Every one runs leader-elected
	// controllers whose stale Lease is a real signal, the loop is skip-if-absent so
	// they cost nothing where they do not exist, and LeaseStale's 4x leaseDuration
	// threshold is generous. TestLeaseCheckUsesTheSharedNamespaceList pins the join.
	read := 0
	for _, ns := range healthNamespaces {
		if !inv.nsExists[ns] {
			continue
		}
		items, listed := kubectlprobe.ListOK[leaseItem]("-n", ns, "get", "leases.coordination.k8s.io")
		if !listed {
			// LISTED SEPARATELY FROM "no stale leases". sectionItems records a
			// CatPending for an unreadable list, which is why the report was not
			// outright green — but this function still went on to record "all
			// controller Leases renewed", a positive claim about namespaces it
			// never read. And that incidental CatPending is a BARE CatPending, so
			// it lands on llz_convergence_state == 2 while LLZClusterNotConverged
			// fires on == 1: in steady-state health it alerts nobody, which
			// contradicts this change's own rule that every softened site goes
			// through PendingIfBudgeted.
			cat, msg := health.PendingIfBudgeted(
				fmt.Sprintf("could not list Leases in %s — retrying against the budget", ns),
				fmt.Sprintf("could not list Leases in %s after retries, so whether its leader-elected "+
					"controllers are still renewing is UNKNOWN", ns))
			record(r, cat, msg)
			continue
		}
		read++
		for _, it := range items {
			if it.Spec.RenewTime == "" {
				continue
			}
			// A RELEASED LEASE IS NOT A STALE ONE, and without this the widening
			// above would abort every converge. Kubernetes leader election clears
			// holderIdentity on a graceful release and leaves renewTime FROZEN at
			// the moment of release — so LeaseStale is permanently true for it,
			// which is CatFail on every pass, which is runConverge's "hard-failed
			// twice in a row — operator intervention required" on a cluster where
			// nothing is wrong. apl-core's namespaces (harbor, istio-system,
			// llz-observability) are exactly where a released lease sits, and they
			// are exactly what this change added.
			if strings.TrimSpace(it.Spec.HolderIdentity) == "" {
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
	// ONLY CLAIM WHAT WAS READ. "all controller Leases renewed" over zero
	// successful lists is the silent green this whole change is about.
	if !stale && read > 0 {
		record(r, health.CatOK, fmt.Sprintf("all controller Leases renewed within 4× leaseDuration (%d namespace(s) read)", read))
	}
}

// fetchArgoApps reads and parses every Application ONCE, before any check runs.
//
// The ownership index has to exist before the FIRST section that judges a
// resource (ExternalSecrets, in checkReadyResources), while the Applications
// section prints much later — so the fetch is split from the classification
// rather than reordering the report. One list, one parse, two consumers.
// fetchArgoApps reads the Applications the ownership index is built from. It runs
// EARLY — the index has to exist before the sections that consult it — but reports
// nothing, because a finding printed here would land under whatever section header
// was last written, ten sections above the one it belongs to. It returns the
// read's success so checkArgoApps can record the inconclusive line under its own
// header, where a reader will look for it.
func fetchArgoApps() ([]health.ArgoApp, bool) {
	raws, ok := kubectlprobe.ListOK[json.RawMessage]("-n", "argocd", "get", "applications.argoproj.io")
	if !ok {
		return nil, false
	}
	var apps []health.ArgoApp
	for _, raw := range raws {
		a, err := health.ParseArgoApp(raw)
		if err != nil {
			continue
		}
		apps = append(apps, a)
	}
	return apps, true
}

func checkArgoApps(r *health.Report, apps []health.ArgoApp, fetched bool, phase1 bool) {
	hdr("ArgoCD Applications")
	if !fetched {
		printFinding(r.RouteInconclusive("ArgoCD Applications"))
	}
	for _, a := range apps {
		recordApp(r, a, phase1)
		// A repo-server↔argocd-redis auth split (WRONGPASS/NOAUTH) makes every app
		// ComparisonError at once; flag it so the converge loop can restart redis
		// once rather than poll to budget exhaustion on a self-inflicted deadlock.
		if health.IsRepoServerCacheAuthError(a.SpecErr) {
			r.RedisAuthSplit = true
		}
		// The git remote refusing Argo's credential is the opposite case: terminal,
		// not transient. Flag it so phase1 can't downgrade it to in-progress and
		// send the gate off to poll a question the remote has already answered.
		//
		// PLATFORM APPLICATIONS ONLY. This flag vetoes the phase1 downgrade for the
		// WHOLE report and prints an ::error:: naming APL_VALUES_REPO_TOKEN, so an
		// app team's unseeded per-app PAT would abort the platform bootstrap and send
		// the operator to the wrong credential — the precise coupling this boundary
		// exists to break, arriving through a flag instead of through a verdict. The
		// two flags below are NOT gated the same way: a repo-server↔redis auth split
		// and an oversized CRD annotation are platform faults that merely happen to be
		// VISIBLE through whichever app noticed first, and both trigger a platform
		// repair rather than a verdict.
		if !health.IsInstanceOwnedApp(a) && health.IsGitAuthError(a.SpecErr) {
			r.GitAuthFailure = true
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

func checkWorkloads(r *health.Report, inv *clusterInventory, owned health.OwnershipIndex, phase1 bool) {
	hdr("Deployments / StatefulSets / DaemonSets")
	checkWorkloadsIn(r, inv, owned, scannedNamespaces(owned), phase1)
}

// checkWorkloadsIn is checkWorkloads over an explicit namespace list — a seam, so
// a test can drive the widened scan over the two namespaces it is about instead of
// stubbing all twelve platform ones to prove a fact about neither.
func checkWorkloadsIn(r *health.Report, inv *clusterInventory, owned health.OwnershipIndex, namespaces []string, phase1 bool) {
	for _, ns := range namespaces {
		if !inv.nsExists[ns] {
			continue
		}
		for _, d := range sectionItems[deploymentItem](r, "Deployments in "+ns, "-n", ns, "get", "deploy") {
			preason, pmsg := progressingCondition(d.Status.Conditions)
			cat, msg := health.ClassifyWorkload("Deployment", ns, d.Metadata.Name, d.Spec.Replicas, d.Status.ReadyReplicas, preason, pmsg, phase1)
			recordRes(r, owned, cat, health.ResourceRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: d.Metadata.Name}, msg)
		}
		for _, s := range sectionItems[statefulSetItem](r, "StatefulSets in "+ns, "-n", ns, "get", "sts") {
			cat, msg := health.ClassifyWorkload("StatefulSet", ns, s.Metadata.Name, s.Spec.Replicas, s.Status.ReadyReplicas, "", "", phase1)
			recordRes(r, owned, cat, health.ResourceRef{Group: "apps", Kind: "StatefulSet", Namespace: ns, Name: s.Metadata.Name}, msg)
		}
		for _, ds := range sectionItems[daemonSetItem](r, "DaemonSets in "+ns, "-n", ns, "get", "ds") {
			cat, msg := health.ClassifyDaemonSet(ns, ds.Metadata.Name, ds.Status.DesiredNumberScheduled, ds.Status.NumberReady, ds.Status.UpdatedNumberScheduled, ds.Status.NumberMisscheduled)
			recordRes(r, owned, cat, health.ResourceRef{Group: "apps", Kind: "DaemonSet", Namespace: ns, Name: ds.Metadata.Name}, msg)
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

func checkPVCs(r *health.Report, owned health.OwnershipIndex) {
	hdr("PersistentVolumeClaim binding")
	for _, p := range sectionItems[pvcItem](r, "PVCs", "get", "pvc", "-A") {
		cat, msg := health.ClassifyPVC(p.Metadata.Namespace, p.Metadata.Name, p.Status.Phase, p.Spec.StorageClassName)
		recordOwned(r, owned, cat, p.Metadata.OwnerReferences,
			health.ResourceRef{Kind: "PersistentVolumeClaim", Namespace: p.Metadata.Namespace, Name: p.Metadata.Name}, msg)
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
	// LLZ's cluster-foundation Application owns the per-namespace default-deny
	// NetworkPolicies. It is ManagedSkip, so on a managed cluster apl-core owns network
	// policy its own way and LLZ applies none — a namespace with no LLZ NPs is then not a
	// failure. Gate the hard-fail on cluster-foundation actually being deployed (self-
	// install); namespaces that DO carry their own NPs still pass either way.
	//
	// ExistsOK, NOT Exists, AND THE DIRECTION OF THE MISTAKE IS THE PROBLEM. Exists
	// folds "the apiserver did not answer" into "absent", and absent here means
	// `ownsNetpols = false`, which REWRITES a genuine missing-default-deny CatFail
	// into CatOK. So the one probe failing did not weaken this section — it deleted
	// its findings and reported them as passes. Fail open on a question, not on an
	// answer.
	ownsNetpols, ownsAnswered := kubectlprobe.ExistsOK("-n", "argocd", "get", "application", "cluster-foundation")
	for _, ns := range healthNamespaces {
		if !inv.nsExists[ns] || health.NetpolExemptNamespace(ns) {
			continue
		}
		// THE MANAGED SKIP IS EVALUATED FIRST, and the order is the point. On a
		// managed cluster — the only supported mode, ADR 0005 — cluster-foundation
		// is ManagedSkip, so LLZ applies no NetworkPolicies and EVERY policy count
		// resolves to CatOK below. Reading the list there cannot change the verdict,
		// so failing on an unreadable read would turn a throttled `get
		// networkpolicies` into a red scheduled health run and burn converge budget
		// for a question whose answer was already fixed.
		//
		// The read still happens — and its failure still matters — on a
		// self-install, where the count decides.
		if ownsAnswered && !ownsNetpols {
			record(r, health.CatOK, fmt.Sprintf("Namespace %s NetworkPolicy check skipped (cluster-foundation not deployed — apl-core owns NPs on managed)", ns))
			continue
		}
		// ItemsOK for the same reason one line down: an unreadable list returns zero
		// items, and zero items is the literal input ClassifyNamespaceNetpol turns
		// into "no default-deny". A read that did not happen must not be graded as a
		// namespace that has no policies.
		nps, listed := kubectlprobe.ItemsOK("-n", ns, "get", "networkpolicies")
		if !listed {
			cat, msg := health.PendingIfBudgeted(
				fmt.Sprintf("Namespace %s NetworkPolicy list unreadable — retrying against the budget", ns),
				fmt.Sprintf("Namespace %s NetworkPolicy list could not be read after retries — this check rendered no verdict, so the default-deny posture of %s is UNKNOWN, not confirmed", ns, ns))
			record(r, cat, msg)
			continue
		}
		cat, msg := health.ClassifyNamespaceNetpol(ns, len(nps))
		if cat == health.CatFail && !ownsNetpols {
			if !ownsAnswered {
				// Cannot tell whether LLZ owns the policies here, so the skip is not
				// available: keep the finding rather than convert it to a pass.
				cat, msg := health.PendingIfBudgeted(
					fmt.Sprintf("%s — and whether cluster-foundation is deployed could not be read, so the managed-cluster skip cannot be applied; retrying against the budget", msg),
					fmt.Sprintf("%s — and whether cluster-foundation is deployed could not be read after retries, so this cannot be dismissed as apl-core owning NPs on a managed cluster", msg))
				record(r, cat, msg)
				continue
			}
			// Unreachable in practice — the answered-and-not-owned case returned
			// above, before the list read. Kept so the CatFail branch stays complete
			// on its own terms rather than depending on a guard 15 lines up.
			record(r, health.CatOK, fmt.Sprintf("Namespace %s NetworkPolicy check skipped (cluster-foundation not deployed — apl-core owns NPs on managed)", ns))
			continue
		}
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

func checkJobs(r *health.Report, owned health.OwnershipIndex, phase1 bool) {
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
		// checkPods skips Job-controlled pods and defers to this section, so if the
		// boundary is not applied here an app team's failed migration Job gates the
		// platform with nothing else able to catch it.
		recordOwned(r, owned, cat, j.Metadata.OwnerReferences,
			health.ResourceRef{Group: "batch", Kind: "Job", Namespace: j.Metadata.Namespace, Name: j.Metadata.Name}, msg)
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

func checkCronWorkflows(r *health.Report, inv *clusterInventory, owned health.OwnershipIndex) {
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
		recordRes(r, owned, cat, health.ResourceRef{Group: "argoproj.io", Kind: "CronWorkflow", Namespace: cw.Metadata.Namespace, Name: cw.Metadata.Name}, msg)
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

func checkServices(r *health.Report, inv *clusterInventory, owned health.OwnershipIndex, phase1 bool) {
	hdr("Service endpoints (repo + instance namespaces)")
	for _, ns := range scannedNamespaces(owned) {
		if !inv.nsExists[ns] {
			continue
		}
		for _, s := range sectionItems[serviceItem](r, "Services in "+ns, "-n", ns, "get", "svc") {
			if s.Spec.Type == "ExternalName" || s.Spec.ClusterIP == "None" {
				continue
			}
			key := ns + "/" + s.Metadata.Name
			p1 := phase1 && health.MatchPrefix(key, health.Phase1PendingWorkloads())
			ready, total := endpointCounts(ns, s.Metadata.Name)
			cat, msg := health.ClassifyServiceEndpoints(key, ready, total, p1)
			if cat != health.CatOK { // only surface non-OK to cut noise (matches script's VERBOSE-gated pass)
				recordRes(r, owned, cat, health.ResourceRef{Kind: "Service", Namespace: ns, Name: s.Metadata.Name}, msg)
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

func checkPDBs(r *health.Report, owned health.OwnershipIndex, phase1 bool) {
	hdr("PodDisruptionBudgets")
	for _, p := range sectionItems[pdbItem](r, "PDBs", "get", "pdb", "-A") {
		key := p.Metadata.Namespace + "/" + p.Metadata.Name
		cat, msg := health.ClassifyPDB(key, p.Status.CurrentHealthy, p.Status.DesiredHealthy, p.Status.DisruptionsAllowed, p.Status.ExpectedPods, phase1)
		if cat != health.CatOK {
			recordRes(r, owned, cat, health.ResourceRef{Group: "policy", Kind: "PodDisruptionBudget", Namespace: p.Metadata.Namespace, Name: p.Metadata.Name}, msg)
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

func checkIngresses(r *health.Report, owned health.OwnershipIndex, phase1 bool) {
	hdr("Ingress addresses")
	for _, ing := range sectionItems[ingressItem](r, "Ingresses", "get", "ingress", "-A") {
		key := ing.Metadata.Namespace + "/" + ing.Metadata.Name
		cat, msg := health.ClassifyIngress(key, len(ing.Status.LoadBalancer.Ingress), phase1)
		recordRes(r, owned, cat, health.ResourceRef{Group: "networking.k8s.io", Kind: "Ingress", Namespace: ing.Metadata.Namespace, Name: ing.Metadata.Name}, msg)
	}
}

// workflowItem is an Argo Workflow reduced to its phase and the two fields that
// say which declared resource it came from — see health.WorkflowDeclaredOwner.
type workflowItem struct {
	meta
	Spec struct {
		WorkflowTemplateRef struct {
			Name         string `json:"name"`
			ClusterScope bool   `json:"clusterScope"`
		} `json:"workflowTemplateRef"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func checkWorkflows(r *health.Report, inv *clusterInventory, owned health.OwnershipIndex, phase1 bool) {
	hdr("Argo Workflows (recent Failed / Error)")
	if !inv.crds["workflows.argoproj.io"] {
		return
	}
	for _, wf := range sectionItems[workflowItem](r, "Workflows", "get", "workflows.argoproj.io", "-A") {
		self := health.ResourceRef{Group: health.WorkflowsGroup, Kind: "Workflow", Namespace: wf.Metadata.Namespace, Name: wf.Metadata.Name}
		// Record the generated Workflow -> declared parent link BEFORE anything
		// consults it, for this section and for the ones that run after. A Workflow
		// carries a generated name no Application declares, and its PODS resolve no
		// further than that name — so without the link both the Workflow and its
		// pods gate the platform. The parent is the CronWorkflow that spawned it or
		// the WorkflowTemplate it was submitted from; either one IS declared.
		if parent, ok := health.WorkflowDeclaredOwner(wf.Metadata.OwnerReferences, wf.Spec.WorkflowTemplateRef.Name,
			wf.Spec.WorkflowTemplateRef.ClusterScope, wf.Metadata.Labels, wf.Metadata.Namespace); ok {
			owned.RecordGenerated(self, parent)
		}
		key := wf.Metadata.Namespace + "/" + wf.Metadata.Name
		if cat, msg := health.ClassifyWorkflowPhase(key, wf.Status.Phase, phase1); cat != health.CatOK {
			// Route on the Workflow itself: Owns resolves the hop just recorded, and
			// a Workflow applied straight from a manifest is declared under this very
			// name, so one ref answers both shapes.
			recordRes(r, owned, cat, self, msg)
		}
	}
}

func checkStuckFinalizers(r *health.Report, inv *clusterInventory, owned health.OwnershipIndex) {
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
				msg := fmt.Sprintf("%s %s/%s stuck Terminating (finalizers: %s)", kind, ns, m.Metadata.Name, strings.Join(m.Metadata.Finalizers, ","))
				// This sweep names a resource of exactly the kinds the boundary
				// covers, so it asks the boundary. A plural with no mapping gates.
				if ref, ok := health.StuckResourceRef(kind, m.Metadata.Namespace, m.Metadata.Name); ok {
					recordOwned(r, owned, health.CatFail, m.Metadata.OwnerReferences, ref, msg)
				} else {
					record(r, health.CatFail, msg)
				}
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

func checkPods(r *health.Report, owned health.OwnershipIndex, phase1 bool) {
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
				recordOwned(r, owned, health.CatPending, p.Metadata.OwnerReferences, health.ResourceRef{Namespace: p.Metadata.Namespace}, detail+" — waiting on OpenBao bootstrap")
			case extDepMatch(key):
				reason, _ := health.MatchExternalDep(key, health.ExternalDepWorkloads())
				recordOwned(r, owned, health.CatDeferred, p.Metadata.OwnerReferences, health.ResourceRef{Namespace: p.Metadata.Namespace}, detail+" — "+reason)
			// Gated on health.Budgeted for the same reason the Service branches are:
			// a pod wedged in ContainerCreating by a FailedMount or
			// FailedAttachVolume never leaves that state, and calling it "still
			// starting" in steady-state health means it never alerts.
			case health.Budgeted && (health.PodIsStarting(p.Status) || health.PodIsWarmingUp(p.Status)):
				// STARTING IS NOT FAILED, and reading PodIsFailing as a verdict
				// cost a release-e2e round: a pod mid-ContainerCreating on a
				// four-minute-old cluster was recorded CatFail, twice sixty
				// seconds apart, and converge aborted with "operator
				// intervention required" while every Application was still
				// flipping OutOfSync -> Synced. PodIsFailing answers "is this
				// pod serving?"; this gate needs "is this pod broken?", and the
				// two differ exactly here. The budget still bounds it — a pod
				// that never starts exhausts the budget and is reported as the
				// timeout it is.
				recordOwned(r, owned, health.CatPending, p.Metadata.OwnerReferences, health.ResourceRef{Namespace: p.Metadata.Namespace}, detail+" — still starting")
			default:
				recordOwned(r, owned, health.CatFail, p.Metadata.OwnerReferences, health.ResourceRef{Namespace: p.Metadata.Namespace}, detail)
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

// endpointCounts returns (ready, total) for a Service's EndpointSlices. Both come
// from ONE list call: two calls could observe different moments of a rollout and
// report ready>total, which would read as nonsense in the message.
func endpointCounts(ns, svc string) (ready, total int) {
	slices := kubectlprobe.List[health.EndpointSlice]("-n", ns, "get", "endpointslices", "-l", "kubernetes.io/service-name="+svc)
	return health.CountReadyEndpoints(slices), health.CountEndpoints(slices)
}

// MOVED HERE from ci_readiness.go rather than injected.
//
// The first draft made this a Deps field, which was the wrong seam: it is a plain
// classified ConfigMap read, and converge already holds cluster-read. Injecting it
// meant the package could not read Loki's config without being handed permission
// to do the thing it is already permitted to do — and the fixture that resulted
// returned "" for every test, so the S3-detection assertions ran against nothing.
// A seam in the wrong place does not just add indirection; it manufactures a
// vacuous fixture.
