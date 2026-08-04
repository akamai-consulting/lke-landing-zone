package main

import (
	"strings"
	"testing"
	"time"
)

// #397 in one test: Ready pods and an S3-shaped config, and not one byte written.
// Before this check, BOTH of assert-loki's conditions passed in exactly that state —
// 238 flush failures on a single ingester, a chunks bucket whose newest object
// predated the cluster by ten days, and a green lane throughout. Checks 1 and 2 are
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
// no signal, and a stray "failed to flush" in an unrelated component must not red
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
// anything. Treating a read error as a failure would red the lane during exactly the
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

// ── the write-OUTCOME half ──────────────────────────────────────────────────

// withLokiWriteProbe stubs the outcome half: what the bucket's newest object is,
// and when the oldest ingester started.
func withLokiWriteProbe(t *testing.T, newest, start time.Time, empty bool) {
	t.Helper()
	oc, op, ol, ocr, on := lokiChunksTarget, lokiPodStart, lokiLogs, objEncConsumerCreds, lokiNow
	os, og := s3SampleObjectKeys, lokiFlushGrace
	t.Cleanup(func() {
		lokiChunksTarget, lokiPodStart, lokiLogs, objEncConsumerCreds, lokiNow = oc, op, ol, ocr, on
		s3SampleObjectKeys, lokiFlushGrace = os, og
	})
	lokiChunksTarget = func() (string, string) { return "platform-loki-chunks-e2e", "us-ord-10.linodeobjects.com" }
	lokiPodStart = func(string, string) (time.Time, error) { return start, nil }
	lokiLogs = func(string, string, time.Duration) (string, error) { return "", nil } // no flush errors
	objEncConsumerCreds = func(string, string, string) (string, string, error) { return "ak", "sk", nil }
	s3SampleObjectKeys = func(_, _, _, _ string, _ int) ([]s3ObjectRef, error) {
		if empty {
			return nil, nil
		}
		return []s3ObjectRef{{Key: "chunk", LastModified: newest}}, nil
	}
	lokiPodsFn = func(string) []lokiPod { return []lokiPod{{ns: "monitoring", name: "loki-ingester-0"}} }
	lokiFlushGrace = func() time.Duration { return 30 * time.Minute }
}

func lokiVerdict(t *testing.T) (fatal bool, text string) {
	t.Helper()
	for _, m := range lokiWriteFindings("loki") {
		text += m.text + "\n"
		if m.fatal {
			fatal = true
		}
	}
	return fatal, text
}

// THE E2E REGRESSION. The gateway goes live during bootstrap and the assert suite
// runs ~9 minutes later, but Loki holds a chunk for up to chunk_idle_period — so a
// perfectly healthy Loki has written nothing yet. Failing there would red every
// fresh cluster, which is exactly the mistake that made the [object] check
// unsatisfiable and is the reason this half is time-aware at all.
func TestLokiWriteOutcomeIsUnprovenNotFailedOnAYoungCluster(t *testing.T) {
	start := time.Date(2026, 8, 3, 19, 41, 0, 0, time.UTC)
	withLokiWriteProbe(t, time.Date(2026, 7, 24, 17, 3, 0, 0, time.UTC), start, false)
	lokiNow = func() time.Time { return start.Add(9 * time.Minute) }

	fatal, text := lokiVerdict(t)
	if fatal {
		t.Errorf("9 minutes in, Loki legitimately may not have flushed — this must not fail:\n%s", text)
	}
	if !strings.Contains(text, "UNPROVEN") {
		t.Errorf("it must SAY it proved nothing rather than reading as a pass:\n%s", text)
	}
}

// Past the grace period, silence is the finding — this is #397 on a real cluster,
// where every other signal stays green while nothing is persisted.
func TestLokiWriteOutcomeFailsOnceAHealthyLokiWouldHaveFlushed(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	withLokiWriteProbe(t, time.Date(2026, 7, 24, 17, 3, 0, 0, time.UTC), start, false)
	lokiNow = func() time.Time { return start.Add(90 * time.Minute) }

	fatal, text := lokiVerdict(t)
	if !fatal {
		t.Errorf("90 minutes with nothing written is not 'too early', it is a write outage:\n%s", text)
	}
	if !strings.Contains(text, "#397") {
		t.Errorf("the finding must point at the known cause:\n%s", text)
	}
}

// The happy path: an object newer than the ingester's start proves persistence.
func TestLokiWriteOutcomePassesWhenSomethingWasWritten(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	withLokiWriteProbe(t, start.Add(20*time.Minute), start, false)
	lokiNow = func() time.Time { return start.Add(90 * time.Minute) }

	fatal, text := lokiVerdict(t)
	if fatal {
		t.Errorf("Loki wrote after the ingesters started; that is the outcome we wanted:\n%s", text)
	}
	if !strings.Contains(text, "has written to object storage") {
		t.Errorf("the pass must name what it observed:\n%s", text)
	}
}

// An empty bucket past the grace is the same outage, and must not crash or pass on
// the "no objects" path.
func TestLokiWriteOutcomeFailsOnAnEmptyBucketPastGrace(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	withLokiWriteProbe(t, time.Time{}, start, true)
	lokiNow = func() time.Time { return start.Add(90 * time.Minute) }

	if fatal, text := lokiVerdict(t); !fatal {
		t.Errorf("an empty chunks bucket 90 minutes in is a write outage:\n%s", text)
	}
}

// A gate that cannot look must not fail the lane — only report that it did not.
func TestLokiWriteOutcomeSkipsWhenItCannotLook(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	withLokiWriteProbe(t, time.Time{}, start, true)
	lokiChunksTarget = func() (string, string) { return "", "" } // no spec / not in an instance repo
	lokiNow = func() time.Time { return start.Add(90 * time.Minute) }

	fatal, text := lokiVerdict(t)
	if fatal {
		t.Errorf("an unmeasurable outcome is not a failed one:\n%s", text)
	}
	if !strings.Contains(text, "SKIP") {
		t.Errorf("it must say it could not measure:\n%s", text)
	}
}

// Flush errors short-circuit: they are the direct evidence, and running the outcome
// half afterwards would add noise to a verdict already reached.
func TestLokiFlushErrorsTakePrecedenceOverTheOutcomeHalf(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	withLokiWriteProbe(t, start.Add(20*time.Minute), start, false)
	lokiNow = func() time.Time { return start.Add(90 * time.Minute) }
	lokiLogs = func(string, string, time.Duration) (string, error) {
		return `level=error msg="failed to flush" err="StatusCode: 403"` + "\n", nil
	}

	fatal, text := lokiVerdict(t)
	if !fatal {
		t.Errorf("an actively failing flush is a failure regardless of older objects:\n%s", text)
	}
	if strings.Contains(text, "UNPROVEN") || strings.Contains(text, "has written to object storage") {
		t.Errorf("the outcome half should not also report once errors decided it:\n%s", text)
	}
}
