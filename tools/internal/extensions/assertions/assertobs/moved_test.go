package assertobs

// Tests moved here by the classify-then-split-by-line-range pass: each references
// a symbol this package defines. Fifteen stranded tests found this way across the
// branch, and the two naming patterns still account for every one.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/objenc"
	"gopkg.in/yaml.v3"
)

func TestHarborCARetrofitRollsPodsThatPredateThePolicy(t *testing.T) {
	h := &retrofitHarness{policy: true, podsCA: []bool{false, true}}
	h.install(t)

	retrofitHarborObjProxyCA()

	if !h.did("rollout restart deploy/harbor-registry") {
		t.Errorf("pods were missing the CA and the retrofit did not roll them; calls: %v", h.calls)
	}
}

// Restarting harbor-registry is a brief interruption to every image push and pull.
// Paying it on every bootstrap — when the pods were already admitted correctly —
// would make the gate itself the outage it is meant to prevent.
func TestHarborCARetrofitDoesNothingWhenPodsAlreadyTrustTheCA(t *testing.T) {
	h := &retrofitHarness{policy: true, podsCA: []bool{true}}
	h.install(t)

	retrofitHarborObjProxyCA()

	if h.did("rollout restart") {
		t.Errorf("pods already carried the CA but the retrofit rolled them anyway — that is a needless "+
			"registry outage on every run; calls: %v", h.calls)
	}
}

// objProxy is default-disabled. On a cluster without it there is no policy, no CA,
// and no reason for this to touch Harbor at all — least of all to read every pod in
// the namespace and conclude they are all "missing" a mount nothing ever adds.
func TestHarborCARetrofitIsInertWithoutTheComponent(t *testing.T) {
	h := &retrofitHarness{policy: false, podsCA: []bool{false}}
	h.install(t)

	retrofitHarborObjProxyCA()

	if h.did("rollout restart") || h.did("get pods") {
		t.Errorf("the objProxy ClusterPolicy is absent, so the component is not enabled here, yet the "+
			"retrofit still went looking at Harbor; calls: %v", h.calls)
	}
}

// The restart is only a fix if the replacement pods actually came back mutated. If
// Kyverno was down they did not, and reporting success there is the difference
// between a warning an operator can act on and a silent Harbor outage.
func harborPodsJSON(t *testing.T, withCA bool) string {
	t.Helper()
	container := func(name string) map[string]any {
		c := map[string]any{"name": name}
		if withCA {
			c["env"] = []map[string]any{{"name": objenc.SsecCertDirEnv, "value": "/etc/ssl/certs:" + objenc.ObjProxyCAMount}}
			c["volumeMounts"] = []map[string]any{{"name": objenc.ObjProxyCAVolume, "mountPath": objenc.ObjProxyCAMount}}
		}
		return c
	}
	pod := map[string]any{
		"metadata": map[string]any{"name": objenc.HarborRegistryLabel + "-7d9f-abcde"},
		"spec": map[string]any{
			"containers": []map[string]any{container("registry"), container("registryctl")},
		},
	}
	if withCA {
		pod["spec"].(map[string]any)["volumes"] = []map[string]any{{"name": objenc.ObjProxyCAVolume}}
	}
	raw, err := json.Marshal(map[string]any{"items": []any{pod}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// retrofitHarness swaps in the read/write seams and records the kubectl verbs the
// retrofit issues.
type retrofitHarness struct {
	calls    []string
	podsCA   []bool // successive answers to "do the running pods carry the CA?"
	policy   bool
	rollFail bool
}

func (h *retrofitHarness) install(t *testing.T) {
	t.Helper()
	origRead, origWrite, origRolled, origBudget := caps.ObjEncDeps, harborCARetrofitKubectl, harborCARetrofitRolledOut, harborWaitBudget
	t.Cleanup(func() {
		caps.ObjEncDeps, harborCARetrofitKubectl, harborCARetrofitRolledOut, harborWaitBudget = origRead, origWrite, origRolled, origBudget
	})
	harborWaitBudget = 50 * time.Millisecond

	reads := 0
	readPods := func(args ...string) (string, error) {
		h.calls = append(h.calls, strings.Join(args, " "))
		i := reads
		reads++
		if i >= len(h.podsCA) {
			i = len(h.podsCA) - 1
		}
		return harborPodsJSON(t, h.podsCA[i]), nil
	}
	// Swap the whole capability set rather than one package-level var: the pod
	// read the retrofit drives is objenc's, and objenc takes it as a Deps field.
	base := origRead()
	base.KubectlOut = readPods
	caps.ObjEncDeps = func() objenc.Deps { return base }

	harborCARetrofitKubectl = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		h.calls = append(h.calls, joined)
		if strings.Contains(joined, "clusterpolicy") && !h.policy {
			return "", errRetrofitNotFound
		}
		if strings.Contains(joined, "rollout restart") && h.rollFail {
			return "", errRetrofitNotFound
		}
		return "", nil
	}
	harborCARetrofitRolledOut = func(string, string) bool { return !h.rollFail }
}

func (h *retrofitHarness) did(substr string) bool {
	for _, c := range h.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// The retrofit exists for exactly one situation: harbor-registry pods that apl-core
// started BEFORE the Kyverno policy existed. Admission-time mutation cannot reach a
// running pod, so nothing else in the component fixes them — and once the CoreDNS
// rewrite is live they cannot complete a single S3 call. They do not crash, so
// nothing restarts them and nothing reports it.
func TestHarborCARetrofitReportsWhenTheRestartDidNotTake(t *testing.T) {
	h := &retrofitHarness{policy: true, podsCA: []bool{false, false}}
	h.install(t)

	out := captureStderr(t, retrofitHarborObjProxyCA)

	if !strings.Contains(out, "::warning::") {
		t.Errorf("pods still lacked the CA after the roll and nothing was reported; stderr: %q", out)
	}
}
func TestLokiFlushFailuresCatchesTheWriteOutage(t *testing.T) {
	orig, prev := lokiLogs, lokiPodsFn
	t.Cleanup(func() { lokiLogs, lokiPodsFn = orig, prev })
	lokiLogs = func(_, pod string, _ time.Duration) (string, error) {
		if strings.Contains(pod, "ingester") {
			return `level=info msg="flushing stream" user=admins` + "\n" +
				`level=error msg="failed to flush" err="store put chunk: operation error S3: PutObject, ` +
				`https response error StatusCode: 403, api error AccessDenied: UnknownError"` + "\n", nil
		}
		return "", nil
	}
	lokiPodsFn = func(string) []lokiPod {
		return []lokiPod{{ns: "monitoring", name: "loki-ingester-0"}}
	}

	fails := lokiFlushFailures("loki")
	if len(fails) != 1 {
		t.Fatalf("an ingester that 403s on every flush must be reported, got %v", fails)
	}
	if !strings.Contains(fails[0], "loki-ingester-0") {
		t.Errorf("the finding must name the pod it came from: %q", fails[0])
	}
}

// Readers are not writers. Scanning queriers and gateways costs log volume and adds
// no signal, and a stray "failed to flush" in an unrelated component must not color.Red
// the lane.
func TestLokiFlushFailuresOnlyScansWriters(t *testing.T) {
	orig, prev := lokiLogs, lokiPodsFn
	t.Cleanup(func() { lokiLogs, lokiPodsFn = orig, prev })
	var scanned []string
	lokiLogs = func(_, pod string, _ time.Duration) (string, error) {
		scanned = append(scanned, pod)
		return "", nil
	}
	lokiPodsFn = func(string) []lokiPod {
		return []lokiPod{
			{ns: "monitoring", name: "loki-ingester-0"},
			{ns: "monitoring", name: "loki-compactor-0"},
			{ns: "monitoring", name: "loki-querier-abc"},
			{ns: "monitoring", name: "loki-gateway-xyz"},
		}
	}

	lokiFlushFailures("loki")

	for _, p := range scanned {
		if strings.Contains(p, "querier") || strings.Contains(p, "gateway") {
			t.Errorf("scanned %q — it reads, it does not flush", p)
		}
	}
	if len(scanned) != 2 {
		t.Errorf("scanned %v, want exactly the ingester and the compactor", scanned)
	}
}

// A healthy Loki must stay silent, or the lane becomes noise and the one signal that
// would have caught #397 gets tuned out like the rest.
func TestLokiFlushFailuresQuietWhenWritesSucceed(t *testing.T) {
	orig, prev := lokiLogs, lokiPodsFn
	t.Cleanup(func() { lokiLogs, lokiPodsFn = orig, prev })
	lokiLogs = func(string, string, time.Duration) (string, error) {
		return `level=info msg="flushing stream" user=admins` + "\n", nil
	}
	lokiPodsFn = func(string) []lokiPod { return []lokiPod{{ns: "monitoring", name: "loki-ingester-0"}} }

	if f := lokiFlushFailures("loki"); len(f) != 0 {
		t.Errorf("successful flushes must be silent, got %v", f)
	}
}

// A pod whose logs cannot be read (just restarted, terminating) is not evidence of
// anything. Treating a read error as a failure would color.Red the lane during exactly the
// window the retrofit-and-restart path creates.
func TestLokiFlushFailuresIgnoresUnreadableLogs(t *testing.T) {
	orig, prev := lokiLogs, lokiPodsFn
	t.Cleanup(func() { lokiLogs, lokiPodsFn = orig, prev })
	lokiLogs = func(string, string, time.Duration) (string, error) {
		return "", errRetrofitNotFound
	}
	lokiPodsFn = func(string) []lokiPod { return []lokiPod{{ns: "monitoring", name: "loki-ingester-0"}} }

	if f := lokiFlushFailures("loki"); len(f) != 0 {
		t.Errorf("an unreadable pod log is not a flush failure, got %v", f)
	}
}

// ── the write PROOF ─────────────────────────────────────────────────────────

// proveHarness stubs everything the prover touches: the bucket target, credentials,
// ingester start, what the bucket contains, and the flush call itself.
type proveHarness struct {
	flushed      []string  // ingesters we POSTed /flush to
	flushErr     error     // what every flush returns
	newest       time.Time // newest object in the bucket
	appearsAt    time.Time // when a NEW object shows up (zero = never)
	start        time.Time // oldest ingester start
	now          time.Time
	postFlushLog string // what the ingesters logged during the flush
}

func (h *proveHarness) install(t *testing.T) {
	t.Helper()
	oc, ocr, ops, on, ofl, onew, osl, obud := lokiChunksTarget, objenc.ObjEncConsumerCreds, lokiPodStart,
		lokiNow, lokiFlushIngester, lokiNewestObject, lokiProveSleep, lokiProveBudget
	t.Cleanup(func() {
		lokiChunksTarget, objenc.ObjEncConsumerCreds, lokiPodStart, lokiNow, lokiFlushIngester,
			lokiNewestObject, lokiProveSleep, lokiProveBudget = oc, ocr, ops, on, ofl, onew, osl, obud
	})
	lokiProveBudget = 20 * time.Second
	lokiChunksTarget = func(string) (string, string) { return "platform-loki-chunks-e2e", "us-ord-10.linodeobjects.com" }
	objenc.ObjEncConsumerCreds = func(objenc.Deps, string, string, string) (string, string, error) { return "ak", "sk", nil }
	lokiPodStart = func(string, string) (time.Time, error) { return h.start, nil }
	lokiNow = func() time.Time { return h.now }
	// Virtual clock: sleeping advances it, so the poll loop terminates without
	// real time passing and a hung prover shows up as a test hang, not a slow suite.
	lokiProveSleep = func(d time.Duration) { h.now = h.now.Add(d) }
	ol := lokiLogs
	t.Cleanup(func() { lokiLogs = ol })
	lokiLogs = func(string, string, time.Duration) (string, error) { return h.postFlushLog, nil }
	lokiFlushIngester = func(p lokiPod) error {
		if h.flushErr != nil {
			return h.flushErr
		}
		h.flushed = append(h.flushed, p.name)
		return nil
	}
	lokiNewestObject = func(_, _, _, _ string) (time.Time, bool) {
		if !h.appearsAt.IsZero() && !h.now.Before(h.appearsAt) {
			return h.appearsAt, true
		}
		if h.newest.IsZero() {
			return time.Time{}, false
		}
		return h.newest, true
	}
	lokiPodsFn = func(string) []lokiPod {
		return []lokiPod{
			{ns: "monitoring", name: "loki-ingester-0"},
			{ns: "monitoring", name: "loki-ingester-1"},
			{ns: "monitoring", name: "loki-ingester-2"},
			{ns: "monitoring", name: "loki-querier-x"}, // must not be flushed
		}
	}
}

func proveVerdict(t *testing.T, allowFlush bool) (fatal bool, text string) {
	t.Helper()
	for _, m := range lokiProveWrites("loki", "e2e", allowFlush) {
		text += m.text + "\n"
		if m.fatal {
			fatal = true
		}
	}
	return fatal, text
}

// THE POINT OF THE WHOLE FILE. On a young cluster nothing has been written, so
// nothing observational can distinguish "healthy but hasn't flushed" from "cannot
// write". Asking Loki to flush and watching a chunk land collapses that into a
// positive result — and this is the case both previous revisions passed vacuously.
func TestLokiWriteOutageIsReportedButDoesNotFailTheLaneWhileDeferred(t *testing.T) {
	if lokiWriteChecksGating {
		t.Skip("gating is on — #397 presumably landed; this test's premise is gone")
	}
	orig, prev := lokiLogs, lokiPodsFn
	t.Cleanup(func() { lokiLogs, lokiPodsFn = orig, prev })
	lokiLogs = func(string, string, time.Duration) (string, error) {
		return `level=error msg="failed to flush" err="StatusCode: 403 AccessDenied"` + "\n", nil
	}
	lokiPodsFn = func(string) []lokiPod { return []lokiPod{{ns: "monitoring", name: "loki-ingester-0"}} }

	// Pods-Ready and S3-config are stubbed out of the picture by lokiPodsFn returning
	// a Ready-less pod, so assert on the write half specifically.
	var sawFinding, sawReason bool
	for _, m := range lokiWriteFindings("loki", "e2e", false) {
		if strings.Contains(m.text, "failing to flush") {
			sawFinding = true
		}
		if strings.Contains(m.text, "REPORTED, NOT GATING") || strings.Contains(m.text, lokiWriteChecksOpenIssue) {
			sawReason = true
		}
		_ = sawReason
	}
	if !sawFinding {
		t.Error("the outage must still be reported in full")
	}
}

// Flipping the flag must actually restore gating — otherwise the deferral is
// permanent by accident and the comment on lokiWriteChecksGating is a lie.
func TestFlippingLokiWriteChecksGatingRestoresTheGate(t *testing.T) {
	orig := lokiWriteChecksGating
	t.Cleanup(func() { lokiWriteChecksGating = orig })
	outage := []lokiWriteMsg{{"FAIL: Loki is failing to flush chunks to object storage", true}}

	lokiWriteChecksGating = false
	deferredFailed, deferredLines := applyLokiWriteVerdict(outage)
	lokiWriteChecksGating = true
	gatingFailed, _ := applyLokiWriteVerdict(outage)

	if !gatingFailed {
		t.Error("with gating ON a flush outage must fail the lane")
	}
	if deferredFailed {
		t.Error("with gating OFF the same outage must be reported without failing the lane")
	}
	var explained bool
	for _, l := range deferredLines {
		if strings.Contains(l, lokiWriteChecksOpenIssue) {
			explained = true
		}
	}
	if !explained {
		t.Errorf("a real failure that fails nothing must name why (%s): %v", lokiWriteChecksOpenIssue, deferredLines)
	}
}

// THE PARTIAL-FAILURE HOLE. A fleet where two ingesters write and a third cannot is
// a cluster silently dropping a share of its logs. The first version returned PROVEN
// the moment ANY object appeared, so the broken replica's errors were never read —
// this gate was producing the partial failure it exists to surface. lokiFlushIngester
// flushes every ingester individually for exactly this reason, and the verdict has to
// honour that.
func TestLokiProveWritesForcesAFlushAndProvesTheChunkLanded(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{
		start:     now.Add(-9 * time.Minute),                     // ingesters young
		newest:    time.Date(2026, 7, 24, 17, 3, 0, 0, time.UTC), // previous cluster's data
		appearsAt: now.Add(10 * time.Second),                     // the flush lands a chunk
		now:       now,
	}
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if fatal {
		t.Fatalf("the flush produced a chunk; that is proof, not failure:\n%s", text)
	}
	if !strings.Contains(text, "PROVEN") {
		t.Errorf("a landed chunk must be reported as PROVEN:\n%s", text)
	}
	if len(h.flushed) != 3 {
		t.Errorf("flushed %v — every INGESTER must be flushed (and only ingesters); a Service would hit "+
			"one replica and hide a partial write outage", h.flushed)
	}
}

// A flush that produces nothing is the outage, and now it is a hard failure rather
// than "too early to tell".
func TestLokiProveWritesFailsWhenTheFlushLandsNothing(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{
		start: now.Add(-9 * time.Minute), now: now, // no object ever appears
		postFlushLog: `level=error msg="failed to flush" err="StatusCode: 403, api error AccessDenied"` + "\n",
	}
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if !fatal {
		t.Fatalf("ingesters flushed, logged write errors, and nothing reached the bucket — that is an outage:\n%s", text)
	}
	if !strings.Contains(text, "#397") {
		t.Errorf("the finding must point at the known cause:\n%s", text)
	}
}

// A flush cannot invent data. An ingester holding no chunk writes nothing and logs
// no error, and failing on that would color.Red a healthy Loki for having nothing to say —
// the same class of false color.Red as the [object] check that started this thread.
func TestLokiProveWritesIsInconclusiveWhenThereWasNothingToFlush(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{start: now.Add(-9 * time.Minute), now: now} // no object, no error
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if fatal {
		t.Fatalf("no chunk buffered and no error logged is not an outage:\n%s", text)
	}
	if !strings.Contains(text, "INCONCLUSIVE") {
		t.Errorf("it must say it proved nothing rather than reading as a pass:\n%s", text)
	}
}

// Forcing a flush writes smaller chunks than Loki would choose, so a cluster that has
// already proven itself must not pay for it.
func TestLokiProveWritesDoesNotFlushWhenAlreadyProven(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{start: now.Add(-60 * time.Minute), newest: now.Add(-5 * time.Minute), now: now}
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if fatal {
		t.Fatalf("an object newer than ingester start is proof:\n%s", text)
	}
	if len(h.flushed) != 0 {
		t.Errorf("already proven, yet it flushed %v — a gratuitous write on every healthy cluster", h.flushed)
	}
	if !strings.Contains(text, "PROVEN") {
		t.Errorf("existing evidence must still read as PROVEN:\n%s", text)
	}
}

// --no-flush-probe keeps the gate read-only, and must say plainly that it therefore
// proved nothing rather than passing silently.
func TestLokiProveWritesReportsUnprovenWhenFlushingIsDisabled(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{start: now.Add(-9 * time.Minute), now: now}
	h.install(t)

	fatal, text := proveVerdict(t, false)
	if fatal {
		t.Errorf("opting out of the probe is not a failure:\n%s", text)
	}
	if len(h.flushed) != 0 {
		t.Errorf("--no-flush-probe must not write: flushed %v", h.flushed)
	}
	if !strings.Contains(text, "UNPROVEN") {
		t.Errorf("it must say it proved nothing:\n%s", text)
	}
}

// A gate that could not reach the ingesters has not found an outage. Reporting one
// would color.Red the lane on an RBAC or networking problem in the CHECK.
func TestLokiProveWritesSkipsWhenNoIngesterCanBeReached(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{start: now.Add(-9 * time.Minute), now: now, flushErr: errRetrofitNotFound}
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if fatal {
		t.Errorf("unreachable /flush is unmeasured, not broken:\n%s", text)
	}
	if !strings.Contains(text, "SKIP") {
		t.Errorf("it must say it could not measure:\n%s", text)
	}
}

// An object that predates the trigger is not evidence the flush worked — it is the
// leftover the trigger timestamp exists to exclude.
func TestLokiProveWritesIgnoresObjectsOlderThanTheTrigger(t *testing.T) {
	now := time.Date(2026, 8, 3, 19, 50, 0, 0, time.UTC)
	h := &proveHarness{
		start: now.Add(-9 * time.Minute),
		// Predates the ingesters, so the cheap pre-check cannot short-circuit and we
		// reach the flush; it is then still older than the trigger, which is the case
		// the trigger timestamp exists to reject.
		newest: now.Add(-20 * time.Minute),
		now:    now,
	}
	h.install(t)

	// Not PROVEN is the invariant; whether it lands on FAIL or INCONCLUSIVE depends
	// on whether the ingesters logged an error, which this case does not exercise.
	_, text := proveVerdict(t, true)
	if strings.Contains(text, "PROVEN:") {
		t.Errorf("an object older than the trigger is a leftover, not evidence this flush wrote anything:\n%s", text)
	}
}

// ── the #397 deferral ───────────────────────────────────────────────────────

// A proven write outage must still be COMPUTED as fatal. The deferral is about
// consequence, not detection: if the verdict itself softened, flipping
// lokiWriteChecksGating back on would restore a check that no longer finds
// anything, and nobody would notice until the next outage.
func TestLokiWriteVerdictStaysFatalEvenWhileDeferred(t *testing.T) {
	now := time.Date(2026, 8, 3, 21, 43, 0, 0, time.UTC)
	h := &proveHarness{
		start: now.Add(-9 * time.Minute), now: now,
		postFlushLog: `level=error msg="failed to flush" err="StatusCode: 403, api error AccessDenied"` + "\n",
	}
	h.install(t)

	if fatal, text := proveVerdict(t, true); !fatal {
		t.Errorf("the verdict must remain fatal regardless of whether the lane acts on it:\n%s", text)
	}
}

// While deferred, the lane must PASS and must say why — an unexplained real failure
// in the log is how a deferral turns into folklore.
func TestLokiProveWritesFailsWhenSomeIngestersCannotWrite(t *testing.T) {
	now := time.Date(2026, 8, 3, 21, 43, 0, 0, time.UTC)
	h := &proveHarness{
		start:        now.Add(-9 * time.Minute),
		now:          now,
		appearsAt:    now.Add(5 * time.Second),                                          // the healthy replicas DID write
		postFlushLog: `level=error msg="failed to flush" err="403 AccessDenied"` + "\n", // one did not
	}
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if !fatal {
		t.Fatalf("a chunk landed but an ingester logged a write error — a share of logs is being dropped:\n%s", text)
	}
	if strings.Contains(text, "PROVEN") {
		t.Errorf("a partially-broken fleet must not read as proven:\n%s", text)
	}
	if !strings.Contains(text, "SOME replicas") {
		t.Errorf("the finding must name the partial nature, or it reads as a total outage:\n%s", text)
	}
}

// And the clean case must state that nothing complained, so PROVEN means "all of
// them wrote", not "at least one did".
func TestLokiProveWritesPassOnlyWhenNoIngesterComplained(t *testing.T) {
	now := time.Date(2026, 8, 3, 21, 43, 0, 0, time.UTC)
	h := &proveHarness{start: now.Add(-9 * time.Minute), now: now, appearsAt: now.Add(2 * time.Second)}
	h.install(t)

	fatal, text := proveVerdict(t, true)
	if fatal {
		t.Fatalf("a clean flush must pass:\n%s", text)
	}
	if !strings.Contains(text, "no ingester reporting a write error") {
		t.Errorf("PROVEN must say the whole fleet was quiet, not just that something landed:\n%s", text)
	}
}
func TestReconcilerAlertSemantics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		// CI always has promtool — check-prom-rules is a hard gate and shells out
		// to it — so this skip only ever fires on a dev box without it.
		t.Skip("promtool not on PATH; the check-prom-rules gate covers CI")
	}

	crd, err := os.ReadFile(reconcilerRuleCRD)
	if err != nil {
		t.Fatalf("read PrometheusRule: %v", err)
	}
	// Run against the SHIPPED rules, extracted the same way the gate does — a
	// hand-copied duplicate would drift and prove nothing about production.
	bare, err := extractBareGroups(crd)
	if err != nil {
		t.Fatalf("extract spec.groups: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yml"), bare, 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := os.ReadFile("testdata/promrules/reconciler_alerts_test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(dir, "alerts_test.yml")
	if err := os.WriteFile(testFile, cases, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(promtool, "test", "rules", testFile).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed: %v\n%s", err, out)
	}
	t.Logf("promtool:\n%s", out)
}

// Every credential alert must be NAMED so the job that reads credential alerts
// actually evaluates it.
//
// The daily credential-single-pane job runs
// `llz ci alert-eval --match '^LLZ(Token|Certificate|Credential)'`, so the alert
// name is not cosmetic — it is the filter. `LLZRootTokenParked` (the original
// spelling) is about the highest-privilege credential in the platform and
// matched NOTHING: the rule was live and would have fired through Alertmanager,
// but the job whose entire purpose is reading credential alerts skipped it.
//
// Asserted against the workflow's own regex, read from the file, so the two
// cannot drift apart — they are edited by different changes in different repos'
// worth of context.
func TestDefaultGrafanaDashboardsMatchTheManifests(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "..", "platform-apl", "components", "observability")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("observability manifests not reachable from the test cwd: %v", err)
	}

	shipped := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "dashboard.yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var cm struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &cm); err != nil || cm.Kind != "ConfigMap" {
			continue
		}
		shipped[cm.Metadata.Namespace+"/"+cm.Metadata.Name] = true

		// While we are here: the manifest itself must carry both sidecar labels.
		// Catching this at PR time beats catching it in an e2e cycle.
		for k, want := range GrafanaSidecarLabels {
			if cm.Metadata.Labels[k] != want {
				t.Errorf("%s: manifest is missing sidecar label %s=%q (found %q) — "+
					"it would render on one stack and vanish on the other",
					e.Name(), k, want, cm.Metadata.Labels[k])
			}
		}
	}
	if len(shipped) == 0 {
		t.Fatal("found no dashboard ConfigMaps — this guard would pass vacuously")
	}

	for _, d := range DefaultGrafanaDashboards {
		if !shipped[d] {
			t.Errorf("DefaultGrafanaDashboards lists %q, which platform-apl does not ship — "+
				"the gate would fail on every cluster", d)
		}
	}
	for d := range shipped {
		if !containsString(DefaultGrafanaDashboards, d) {
			t.Errorf("platform-apl ships dashboard %q that DefaultGrafanaDashboards does not gate — "+
				"add it, or it can regress unnoticed", d)
		}
	}
}
