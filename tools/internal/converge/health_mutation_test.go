package converge

// ci_health_mutation_test.go pins the behaviours of the convergence gate that
// ci_health_test.go exercises but does not ASSERT — the axes on which a mutated
// gate still passed the suite. Each test here names the distinction it defends:
// how many retries a probe spends, which pods/kinds a loop actually visits, and
// which of two categories a finding lands in. A gate that gets any of these
// wrong cannot tell a healthy cluster from an unhealthy one on that axis.
//
// Several findings the gate emits are CatOK/CatWarn, which health.Report
// deliberately does not bucket (contract.go: "CatOK is a pass"), so the only
// observable is the line record() prints — hence the captureStdout assertions.

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// ── converge loop: budget arithmetic + poll numbering (runConverge) ───────────

// The deadline is budget SECONDS from now, and the poll counter counts UP.
//
// Both are invisible to a single-poll test: with the budget computed in
// nanoseconds instead of seconds the deadline is already past when the first
// poll returns, so a cluster that is merely in-progress is reported as "budget
// exhausted" on poll 1 — converge would stop waiting for every cluster that has
// not converged instantly. The fixture below therefore needs at least two polls:
// poll 1 is pre-bootstrap (exit 2 — keep polling), poll 2 hard-fails, and the
// hard-fail re-check makes it terminal, so the run must end on the hard-fail
// verdict rather than on a budget it has nowhere near spent.
func TestRunConvergeBudgetIsSecondsAndPollsCountUp(t *testing.T) {
	poll := 0
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "version --request-timeout=10s":
			poll++
			return nil, nil
		case "get crd -o json":
			if poll == 1 {
				// Pre-bootstrap: applications.argoproj.io absent => exit 2 (poll on).
				return items(), nil
			}
			// Only the Argo CRD is installed, so checkRequiredCRDs hard-fails on
			// the rest => exit 1.
			return items(`{"metadata":{"name":"applications.argoproj.io"}}`), nil
		case "-n argocd get application platform-bootstrap":
			return nil, nil
		case "get ns -o json":
			return items(`{"metadata":{"name":"argocd"},"status":{"phase":"Active"}}`), nil
		case "-n cert-manager get secret platform-app-ca":
			return nil, nil // present => phase1 over, so the hard fails are not demoted
		}
		return nil, errors.New("nope")
	})

	var err error
	// budget=3600s is nowhere near spent; interval/retry-delay 0 keep it instant.
	stderr := captureStderr(t, func() { err = runConverge(3600, 0, 0) })

	if err == nil {
		t.Fatalf("runConverge = nil, want the hard-fail verdict (polls seen: %d)", poll)
	}
	if strings.Contains(err.Error(), "budget") {
		t.Errorf("runConverge ended on a budget it had 3600s of: %v — the deadline must be budget SECONDS from now, not nanoseconds", err)
	}
	if !strings.Contains(err.Error(), "hard-failed twice in a row") {
		t.Errorf("runConverge = %v, want the twice-in-a-row hard-fail abort", err)
	}
	if poll < 3 {
		t.Errorf("health ran %d times, want 3 (in-progress poll, hard-fail poll, hard-fail re-check)", poll)
	}
	// The poll counter is what the operator correlates the log against and what
	// the long-pole report cites; it must ascend.
	for _, want := range []string{"convergence poll attempt 1", "convergence poll attempt 2"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q — poll numbering must count up.\n%s", want, stderr)
		}
	}
}

// ── converge long-pole: the step-summary write is reported only when it FAILS ─

func TestReportConvergeLongPoleWarnsOnlyOnWriteFailure(t *testing.T) {
	// No GITHUB_STEP_SUMMARY => deps.Summary is a no-op returning nil. A warning
	// here would tell an operator the report was lost when it was never asked for.
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	quiet := captureStderr(t, func() { reportConvergeLongPole([]string{"monitoring-loki"}, 7) })
	if strings.Contains(quiet, "step-summary write failed") {
		t.Errorf("a successful (skipped) summary write warned anyway:\n%s", quiet)
	}
	if !strings.Contains(quiet, "still not-OK on poll 7") {
		t.Errorf("long-pole notice missing:\n%s", quiet)
	}

	// A genuinely unwritable path must warn — best-effort, but not silent.
	t.Setenv("GITHUB_STEP_SUMMARY", t.TempDir()+"/no-such-dir/summary.md")
	loud := captureStderr(t, func() { reportConvergeLongPole([]string{"monitoring-loki"}, 7) })
	if !strings.Contains(loud, "step-summary write failed") {
		t.Errorf("a failed summary write was swallowed:\n%s", loud)
	}
}

// ── argocd-redis realign: the rollout-status wait warns only when it fails ────

func TestRealignArgocdRedisWarnsOnlyOnStatusFailure(t *testing.T) {
	withKubectl(t, func(string) ([]byte, error) { return nil, nil })
	quiet := captureStderr(t, realignArgocdRedis)
	if strings.Contains(quiet, "status wait failed") {
		t.Errorf("a successful rollout status reported a wait failure:\n%s", quiet)
	}

	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "rollout status") {
			return nil, errors.New("timed out waiting for the condition")
		}
		return nil, nil
	})
	loud := captureStderr(t, realignArgocdRedis)
	if !strings.Contains(loud, "status wait failed") {
		t.Errorf("a failed rollout status was swallowed — converge keeps polling blind:\n%s", loud)
	}
}

// ── ClusterSecretStore probe: retry budget and where the pause sits ───────────

// The phase1 ClusterSecretStore probe retries probeRetries TIMES with a pause
// BETWEEN attempts. Both halves matter to the gate: the count bounds how long
// every phase1 poll costs (converge drops it to 1 precisely because it is a
// multiplier), and a pause after the FINAL attempt is pure dead time paid on
// every poll of a cluster that is legitimately not ready yet.
func TestOpenBaoClusterSecretStoreRetryBudgetAndPauseSpacing(t *testing.T) {
	origRetries, origDelay := kubectlprobe.Retries, kubectlprobe.Delay
	t.Cleanup(func() { kubectlprobe.Retries, kubectlprobe.Delay = origRetries, origDelay })
	kubectlprobe.Retries, kubectlprobe.Delay = 3, 40*time.Millisecond

	var at []time.Time
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get clustersecretstore "+defaultSecretStore+" -o json" {
			return nil, errors.New("unexpected " + a)
		}
		at = append(at, time.Now())
		return []byte(`{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`), nil
	})

	if openBaoClusterSecretStoreReadyWithRetry() {
		t.Fatal("a not-Ready ClusterSecretStore must not read as ready")
	}
	done := time.Now()

	if len(at) != kubectlprobe.Retries {
		t.Fatalf("probe ran %d times, want probeRetries=%d", len(at), kubectlprobe.Retries)
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < kubectlprobe.Delay*3/4 {
			t.Errorf("attempts %d→%d were %v apart, want a ≥%v pause — retrying with no pause re-asks the same instant",
				i-1, i, gap, kubectlprobe.Delay)
		}
	}
	if tail := done.Sub(at[len(at)-1]); tail > kubectlprobe.Delay/2 {
		t.Errorf("slept %v after the FINAL attempt — the pause belongs between attempts, not before the return", tail)
	}
}

// ── record(): an unknown category prints blank, it does not blow up the scan ──

func TestRecordUnknownCategoryPrintsBlankLabel(t *testing.T) {
	var r health.Report
	out := captureStdout(t, func() {
		record(&r, health.CatFail, "known-category")
		// Not in catStyles: zero-value style, so there is no colorizer to call.
		// Reaching for one aborts the whole health scan on the first such finding.
		record(&r, health.Category(99), "unknown-category")
	})
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "known-category") {
		t.Errorf("known category lost its label:\n%s", out)
	}
	if !strings.Contains(out, "unknown-category") {
		t.Errorf("unknown category was not printed:\n%s", out)
	}
}

// ── checkNodes: a taint's value is only appended when it HAS one ──────────────

func TestCheckNodesTaintValueRendering(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get nodes -o json" {
			return nil, errors.New("nope")
		}
		return items(`{"metadata":{"name":"n1"},"spec":{"taints":[` +
			`{"key":"dedicated","value":"gpu","effect":"NoSchedule"},` +
			`{"key":"quarantine","effect":"NoExecute"}` +
			`]},"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"False"},{"type":"PIDPressure","status":"False"}]}}`), nil
	})
	var r health.Report
	checkNodes(&r)
	joined := strings.Join(r.Failed, "\n")
	if !strings.Contains(joined, "taint dedicated=gpu:NoSchedule") {
		t.Errorf("valued taint must render key=value:effect, got:\n%s", joined)
	}
	if !strings.Contains(joined, "taint quarantine:NoExecute") || strings.Contains(joined, "quarantine=:") {
		t.Errorf("valueless taint must render key:effect with no '=', got:\n%s", joined)
	}
}

// ── checkLokiObjStorage: "no config" and "config without S3" are not the same ─

func TestCheckLokiObjStorageEmptyVsRenderedConfig(t *testing.T) {
	serve := func(cms ...string) func(string) ([]byte, error) {
		return func(a string) ([]byte, error) {
			switch a {
			case "get secret obj-secrets -n apl-secrets":
				return nil, nil // obj configured => the section gates
			case "get configmap -A -o json":
				return items(cms...), nil
			}
			return nil, errors.New("nope")
		}
	}

	// No Loki ConfigMap at all: nothing to await — a clean pass, never a poll.
	withKubectl(t, serve())
	var absent health.Report
	out := captureStdout(t, func() { checkLokiObjStorage(&absent, false) })
	if len(absent.Pending) != 0 {
		t.Errorf("an undeployed Loki must not hold the gate open: pending %v", absent.Pending)
	}
	if !strings.Contains(out, "Loki not deployed") {
		t.Errorf("expected the not-deployed pass, got:\n%s", out)
	}

	// A rendered config that DOES reference S3 is the converged case, and must be
	// reported as such rather than as "not deployed".
	withKubectl(t, serve(`{"metadata":{"name":"loki"},"data":{"config.yaml":"storage_config:\n  object_store: s3\n"}}`))
	var s3 health.Report
	out = captureStdout(t, func() { checkLokiObjStorage(&s3, false) })
	if len(s3.Pending) != 0 {
		t.Errorf("an S3-backed Loki is converged: pending %v", s3.Pending)
	}
	if !strings.Contains(out, "references S3") {
		t.Errorf("a rendered S3 config must be recognised, got:\n%s", out)
	}

	// A rendered config still on the filesystem backend is the POLL case.
	withKubectl(t, serve(`{"metadata":{"name":"loki"},"data":{"config.yaml":"storage_config:\n  object_store: filesystem\n"}}`))
	var fs health.Report
	checkLokiObjStorage(&fs, false)
	if len(fs.Pending) != 1 {
		t.Errorf("a non-S3 Loki must keep converge polling: pending %v", fs.Pending)
	}
}

// ── checkFirewallBootstrap: a populated config key passes, an empty one defers ─

func TestCheckFirewallBootstrapConfigKeyCategories(t *testing.T) {
	serve := func(vals map[string]string) func(string) ([]byte, error) {
		return func(a string) ([]byte, error) {
			switch {
			case a == "-n kube-system get deployment "+deps.FirewallDeploymentName:
				// Absent (self-discovery-only cluster) => no token assertion, so the
				// ConfigMap keys are the only findings this section can produce.
				return nil, errors.New("NotFound")
			case a == "-n kube-system get configmap "+deps.FirewallConfigMapName:
				return nil, nil
			case strings.HasPrefix(a, "-n kube-system get configmap "+deps.FirewallConfigMapName+" -o jsonpath={.data."):
				key := strings.TrimSuffix(strings.TrimPrefix(a, "-n kube-system get configmap "+deps.FirewallConfigMapName+" -o jsonpath={.data."), "}")
				return []byte(vals[key]), nil
			}
			return nil, errors.New("nope")
		}
	}

	// Fully populated ConfigMap: every key passes, nothing is deferred.
	withKubectl(t, serve(map[string]string{
		"LINODE_FIREWALL_ID": "123", "LKE_CLUSTER_ID": "456", "FIREWALL_TEMPLATE_ID": "789",
		"RECONCILE_INTERVAL_SECS": "300", "VPC_CIDR": "10.0.0.0/16",
	}))
	var full health.Report
	out := captureStdout(t, func() { checkFirewallBootstrap(&full) })
	if len(full.Deferred) != 0 {
		t.Errorf("a fully-populated firewall ConfigMap deferred %v — a set key is a pass", full.Deferred)
	}
	if !strings.Contains(out, "VPC_CIDR = 10.0.0.0/16") {
		t.Errorf("a set key must be reported with its value, got:\n%s", out)
	}

	// One unset key (and LKE_CLUSTER_ID, which passes even when empty): exactly
	// one deferral, naming the key that is actually missing.
	withKubectl(t, serve(map[string]string{
		"LINODE_FIREWALL_ID": "123", "FIREWALL_TEMPLATE_ID": "789", "RECONCILE_INTERVAL_SECS": "300",
	}))
	var partial health.Report
	checkFirewallBootstrap(&partial)
	if len(partial.Deferred) != 1 || !strings.Contains(partial.Deferred[0], "VPC_CIDR") {
		t.Errorf("deferred = %v, want exactly the empty VPC_CIDR key", partial.Deferred)
	}
}

// ── checkOpenBao: the pod loop visits exactly .spec.replicas pods ─────────────

// baoPods serves a 3-replica OpenBao STS whose pods all exist, are Ready, and
// report the given HA modes. Anything outside that set errors, so a loop that
// walks past the last replica reports the extra pod MISSING — a hard failure
// against a perfectly healthy cluster.
func baoPods(t *testing.T, replicas int, haMode map[int]string) {
	t.Helper()
	withKubectl(t, func(a string) ([]byte, error) {
		switch {
		case strings.Contains(a, "get sts platform-openbao"):
			return []byte(strconv.Itoa(replicas)), nil
		case strings.Contains(a, "containerStatuses"):
			return []byte("true"), nil
		case strings.Contains(a, "bao status"):
			for i := 0; i < replicas; i++ {
				if !strings.Contains(a, "exec platform-openbao-"+string(rune('0'+i))+" ") {
					continue
				}
				if haMode[i] == "active" {
					return []byte(`{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`), nil
				}
				return []byte(`{"initialized":true,"sealed":false,"ha_enabled":true}`), nil
			}
			return nil, errors.New("no such pod")
		case strings.HasPrefix(a, "-n "+OpenbaoNamespace+" get pod platform-openbao-"):
			idx := strings.TrimPrefix(a, "-n "+OpenbaoNamespace+" get pod platform-openbao-")
			if n := int(idx[0] - '0'); len(idx) == 1 && n >= 0 && n < replicas {
				return []byte("ok"), nil
			}
			return nil, errors.New(`Error from server (NotFound): pods "` + idx + `" not found`)
		}
		return nil, errors.New("unexpected " + a)
	})
}

func TestCheckOpenBaoVisitsExactlyReplicaPods(t *testing.T) {
	baoPods(t, 3, map[int]string{0: "active"})
	var r health.Report
	// phase1=true skips the per-pod audit-device exec; the pod loop is the subject.
	checkOpenBao(&r, true)
	if len(r.Failed) != 0 {
		t.Errorf("a healthy 3-replica OpenBao hard-failed: %v — the loop must visit pods 0..replicas-1 only", r.Failed)
	}
	for _, p := range append(append([]string{}, r.Failed...), r.Pending...) {
		if strings.Contains(p, "platform-openbao-3") {
			t.Errorf("the loop walked past the last replica: %q", p)
		}
	}
}

// A leader-count verdict is recorded only when it is NOT OK. Inverting that
// silences "no active leader" — the split-brain / leaderless check disappears
// while a color.Green line appears in its place.
func TestCheckOpenBaoRecordsLeaderCountOnlyWhenNotOK(t *testing.T) {
	// Every pod unsealed but standby => zero active leaders => hard fail.
	baoPods(t, 3, map[int]string{})
	var leaderless health.Report
	checkOpenBao(&leaderless, true)
	found := false
	for _, f := range leaderless.Failed {
		if strings.Contains(f, "no active leader") {
			found = true
		}
	}
	if !found {
		t.Errorf("a leaderless OpenBao did not hard-fail: failed %v pending %v", leaderless.Failed, leaderless.Pending)
	}

	// Exactly one leader => the OK verdict is not worth a line.
	baoPods(t, 3, map[int]string{1: "active"})
	var healthy health.Report
	out := captureStdout(t, func() { checkOpenBao(&healthy, true) })
	if strings.Contains(out, "exactly one active OpenBao leader") {
		t.Errorf("the OK leader-count verdict should stay unrecorded:\n%s", out)
	}
}

// ── checkPVs: the Released roll-up fires only when there ARE Released PVs ─────

func TestCheckPVsReleasedRollup(t *testing.T) {
	serve := func(blobs ...string) func(string) ([]byte, error) {
		return func(a string) ([]byte, error) {
			if a != "get pv -o json" {
				return nil, errors.New("nope")
			}
			return items(blobs...), nil
		}
	}

	// All Bound: the clean line, not a "0 Released PV(s)" warning that would send
	// an operator to run orphan-cleanup against nothing.
	withKubectl(t, serve(`{"metadata":{"name":"pv1"},"status":{"phase":"Bound"}}`))
	var clean health.Report
	out := captureStdout(t, func() { checkPVs(&clean) })
	if !strings.Contains(out, "no Released/Failed/Pending PVs") {
		t.Errorf("expected the clean PV line, got:\n%s", out)
	}
	if strings.Contains(out, "Released PV(s)") {
		t.Errorf("a cluster with no Released PVs raised the orphan-cleanup warning:\n%s", out)
	}

	// One Released: the roll-up must appear, counting it.
	withKubectl(t, serve(
		`{"metadata":{"name":"pv1"},"status":{"phase":"Bound"}}`,
		`{"metadata":{"name":"pv2"},"status":{"phase":"Released"}}`,
	))
	var leaked health.Report
	out = captureStdout(t, func() { checkPVs(&leaked) })
	if !strings.Contains(out, "1 Released PV(s)") {
		t.Errorf("a Released PV must be rolled up, got:\n%s", out)
	}
	if strings.Contains(out, "no Released/Failed/Pending PVs") {
		t.Errorf("a Released PV was reported as a clean run:\n%s", out)
	}
}

// ── checkNetworkPolicies: the managed-cluster excuse applies to FAILURES only ─

func TestCheckNetworkPoliciesExcusesOnlyFailures(t *testing.T) {
	serve := func(foundation bool) func(string) ([]byte, error) {
		return func(a string) ([]byte, error) {
			switch a {
			case "get ns -o json":
				return items(`{"metadata":{"name":"harbor"},"status":{"phase":"Active"}}`), nil
			case "get crd -o json":
				return items(), nil
			case "-n argocd get application cluster-foundation":
				if !foundation {
					return nil, errors.New("NotFound")
				}
				return nil, nil
			case "-n harbor get networkpolicies -o json":
				return items(), nil // zero NPs => ClassifyNamespaceNetpol hard-fails
			}
			return nil, errors.New("nope")
		}
	}

	// cluster-foundation absent (managed): LLZ owns no NPs, so a namespace with
	// none is not LLZ's failure.
	withKubectl(t, serve(false))
	var managed health.Report
	checkNetworkPolicies(&managed, mustInventory(t))
	if len(managed.Failed) != 0 {
		t.Errorf("a managed cluster hard-failed on absent LLZ NetworkPolicies: %v", managed.Failed)
	}

	// cluster-foundation deployed (self-install): the same namespace IS a failure,
	// so the excuse must not be a blanket amnesty.
	withKubectl(t, serve(true))
	var selfInstall health.Report
	checkNetworkPolicies(&selfInstall, mustInventory(t))
	if len(selfInstall.Failed) != 1 || !strings.Contains(selfInstall.Failed[0], "default-deny missing") {
		t.Errorf("a self-installed cluster missing default-deny must hard-fail, got %v", selfInstall.Failed)
	}
}

// ── checkJobs: a Complete/True condition is what makes a Job complete ─────────

func TestCheckJobsReadsCompleteCondition(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a != "get jobs -A -o json" {
			return nil, errors.New("nope")
		}
		return items(`{"metadata":{"namespace":"llz-openbao","name":"bootstrap-openbao"},` +
			`"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`), nil
	})
	var r health.Report
	out := captureStdout(t, func() { checkJobs(&r, false) })
	if !strings.Contains(out, "Job llz-openbao/bootstrap-openbao Complete (1 succeeded)") {
		t.Errorf("a Complete=True Job must read as Complete, got:\n%s", out)
	}
	if strings.Contains(out, "in progress") {
		t.Errorf("a finished Job was reported as still running:\n%s", out)
	}
}

// ── checkStuckFinalizers: which kinds are scanned, and at which scope ─────────

// pv/pvc are core kinds — always present, never in the CRD inventory — so they
// are exempt from the CRD gate; every other kind is skipped unless its CRD is
// installed. And each kind is listed at its own scope: pvc is namespaced (-A),
// pv is cluster-scoped. Getting either wrong means the section silently lists
// nothing (a `kubectl get` for an uninstalled CRD errors) and reports the clean
// "no resources stuck Terminating" line for a cluster it never looked at.
func TestCheckStuckFinalizersKindGateAndScope(t *testing.T) {
	var calls []string
	withKubectl(t, func(a string) ([]byte, error) {
		switch a {
		case "get crd -o json", "get ns -o json":
			return items(), nil // no CRDs installed at all
		}
		calls = append(calls, a)
		return items(), nil
	})
	var r health.Report
	checkStuckFinalizers(&r, mustInventory(t))

	want := []string{"get pv -o json", "get -A pvc -o json"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("stuck-finalizer scan issued\n  %v\nwant\n  %v\n(pv/pvc only — they are core kinds, not CRDs — each at its own scope)", calls, want)
	}
}

// ── checkPods: the flapping-container warning needs actual flapping ───────────

func TestCheckPodsFlappingWarning(t *testing.T) {
	serve := func(restarts int) func(string) ([]byte, error) {
		return func(a string) ([]byte, error) {
			if a != "get pods -A -o json" {
				return nil, errors.New("nope")
			}
			return items(`{"metadata":{"namespace":"harbor","name":"harbor-core"},"status":{"phase":"Running","containerStatuses":[` +
				`{"name":"core","ready":true,"restartCount":` + strconv.Itoa(restarts) + `}]}}`), nil
		}
	}

	// Well under the threshold: no warning, and none with an empty container list.
	withKubectl(t, serve(0))
	var calm health.Report
	out := captureStdout(t, func() { checkPods(&calm, false) })
	if strings.Contains(out, "flapping containers") {
		t.Errorf("a stable pod was reported as flapping:\n%s", out)
	}

	// Over the threshold: the warning must name the container and its count.
	withKubectl(t, serve(9))
	var hot health.Report
	out = captureStdout(t, func() { checkPods(&hot, false) })
	if !strings.Contains(out, "flapping containers: core=9") {
		t.Errorf("a restart-looping container went unreported:\n%s", out)
	}
}
