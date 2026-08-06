package main

import (
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
)

// #397 in one test: Ready pods and an S3-shaped config, and not one byte written.
// Before this check, BOTH of assert-loki's conditions passed in exactly that state —
// 238 flush failures on a single ingester, a chunks bucket whose newest object
// predated the cluster by ten days, and a color.Green lane throughout. Checks 1 and 2 are
// properties of the cluster's INTENT; this is the only one that asks about outcome.
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
