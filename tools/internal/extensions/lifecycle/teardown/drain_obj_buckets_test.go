package teardown

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
)

// drain-obj-buckets refuses without --yes, deliberately: it deletes every log chunk
// and registry blob a deployment owns. The workflow step that calls it was written
// WITHOUT the flag, so on its first real run the verb did exactly what it promised —
// refused — and failed teardown instead of draining. The guard worked; the call site
// had never been exercised.
//
// A workflow cannot be unit-tested, so this asserts the invocation directly: any
// `ci drain-obj-buckets` in a shipped workflow must carry --yes.
func TestWorkflowsInvokeDrainObjBucketsWithYes(t *testing.T) {
	const wf = "../../../../../instance-template/.github/workflows/llz-terraform.yml"
	raw, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("could not read %s (%v) — a skip here would reproduce the gap it closes", wf, err)
	}
	calls := regexp.MustCompile(`(?m)^.*\bci drain-obj-buckets\b.*$`).FindAllString(string(raw), -1)
	if len(calls) == 0 {
		t.Fatal("no drain-obj-buckets invocation found — if the step was removed, remove this test with it")
	}
	for _, line := range calls {
		if !strings.Contains(line, "--yes") {
			t.Errorf("drain-obj-buckets invoked without --yes, so it will refuse at runtime and fail the "+
				"job rather than drain:\n  %s", strings.TrimSpace(line))
		}
	}
}

// And the refusal itself must stay: the workflow gate (drain_data_buckets, default
// false) and this flag are independent, so losing either leaves one deliberate step
// between a routine destroy and erasing the logs.
func TestDrainObjBucketsRefusesWithoutYes(t *testing.T) {
	err := RunDrainObjBuckets("e2e", false)
	if err == nil {
		t.Fatal("drain-obj-buckets must refuse without --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal must name the flag it wants: %v", err)
	}
}

// A missing --region must fail before anything is deleted, not default to a
// deployment the caller did not name.
func TestDrainObjBucketsRequiresARegion(t *testing.T) {
	if err := RunDrainObjBuckets("", true); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("empty region must be rejected, got %v", err)
	}
}

// withDrainStubs fakes the LIST/DELETE pair so the convergence logic is testable.
func withDrainStubs(t *testing.T, remaining *int, survivePerRound int) *int {
	t.Helper()
	oList, oDel, oSleep := objstore.SampleObjectKeys, s3DeleteObjects, drainSleep
	t.Cleanup(func() { objstore.SampleObjectKeys, s3DeleteObjects, drainSleep = oList, oDel, oSleep })
	drainSleep = func(time.Duration) {}
	rounds := 0
	objstore.SampleObjectKeys = func(_, _, _, _ string, _ int) ([]objstore.ObjectRef, error) {
		out := make([]objstore.ObjectRef, 0, *remaining)
		for i := 0; i < *remaining; i++ {
			out = append(out, objstore.ObjectRef{Key: fmt.Sprintf("k%d", i)})
		}
		return out, nil
	}
	s3DeleteObjects = func(_, _, _, _ string, keys []string) (int, error) {
		rounds++
		survived := survivePerRound
		if survived > len(keys) {
			survived = len(keys)
		}
		*remaining = survived
		return survived, nil
	}
	return &rounds
}

// Ceph returns a transient per-key InternalError under load — observed mid-drain on
// a bucket whose sibling emptied cleanly with the same credential. Treating any
// <Error> as fatal aborted the whole drain on one unlucky object and declared that
// the next cluster would inherit unreadable data, when a retry would have removed it.
func TestDrainRetriesKeysThatSurviveATransientDeleteError(t *testing.T) {
	remaining := 3
	calls := 0
	oList, oDel, oSleep := objstore.SampleObjectKeys, s3DeleteObjects, drainSleep
	t.Cleanup(func() { objstore.SampleObjectKeys, s3DeleteObjects, drainSleep = oList, oDel, oSleep })
	drainSleep = func(time.Duration) {}
	objstore.SampleObjectKeys = func(_, _, _, _ string, _ int) ([]objstore.ObjectRef, error) {
		out := make([]objstore.ObjectRef, 0, remaining)
		for i := 0; i < remaining; i++ {
			out = append(out, objstore.ObjectRef{Key: fmt.Sprintf("k%d", i)})
		}
		return out, nil
	}
	s3DeleteObjects = func(_, _, _, _ string, keys []string) (int, error) {
		calls++
		if calls == 1 {
			remaining = 1 // one key hit InternalError and survived
			return 1, nil
		}
		remaining = 0
		return 0, nil
	}

	n, err := drainOneBucket("ak", "sk", "ep", "b")
	if err != nil {
		t.Fatalf("a transient per-key failure must be retried, not fatal: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted %d, want all 3 across the two passes", n)
	}
}

// But an object that will not go must eventually be reported, not spun on for the
// whole page budget.
func TestDrainGivesUpOnObjectsThatNeverDelete(t *testing.T) {
	remaining := 2
	rounds := withDrainStubs(t, &remaining, 2) // every key survives, every round

	_, err := drainOneBucket("ak", "sk", "ep", "b")
	if err == nil {
		t.Fatal("objects that never delete must fail the drain")
	}
	if !strings.Contains(err.Error(), "consecutive") {
		t.Errorf("the error must say it stopped making progress: %v", err)
	}
	if *rounds > drainMaxStalledRounds+1 {
		t.Errorf("spun %d rounds; the stall bail-out should stop near %d", *rounds, drainMaxStalledRounds)
	}
}

// The REAL DeleteObjects parser, not a stub of it. Ceph answers Quiet-mode bulk
// deletes with per-key <Error> entries for transient InternalErrors; those must come
// back as a SURVIVOR COUNT the caller can retry, not as a hard error that aborts the
// drain. Stubbing s3DeleteObjects to test convergence leaves this parsing untested,
// which is how the fatal-on-any-<Error> behaviour survived a mutant.
func TestDeleteObjectsReportsSurvivorsRatherThanFailing(t *testing.T) {
	prev := s3PostWithBody
	t.Cleanup(func() { s3PostWithBody = prev })

	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, bool, error) {
		return 200, `<DeleteResult><Error><Code>InternalError</Code><Key>admins/abc</Key></Error></DeleteResult>`, false, nil
	}
	survived, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"admins/abc", "other"})
	if err != nil {
		t.Fatalf("a transient per-key InternalError must not fail the batch: %v", err)
	}
	if survived != 1 {
		t.Errorf("survivors = %d, want 1 so the caller retries exactly what is left", survived)
	}

	// A clean response reports nothing surviving.
	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, bool, error) {
		return 200, `<DeleteResult></DeleteResult>`, false, nil
	}
	if survived, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"k"}); err != nil || survived != 0 {
		t.Errorf("clean delete = (%d, %v), want (0, nil)", survived, err)
	}

	// A transport/HTTP failure is still a hard error — that is not per-key.
	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, bool, error) {
		return 503, "upstream unavailable", false, nil
	}
	if _, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"k"}); err == nil {
		t.Error("an HTTP 503 must fail the batch rather than read as zero survivors")
	}
}

// TestDeleteObjectsWillNotCountATruncatedBody. The survivor count is derived from
// the response, and the response used to be read with a 16 KiB cap and then
// treated as complete. A DeleteObjects reply saying all 1000 keys failed runs to
// roughly 200 KB — so a batch that deleted NOTHING was read as about 80 failures,
// i.e. ~920 deleted. `deleted == 0` never tripped, the stall detector never fired,
// the total was inflated by objects still sitting in the bucket, and the drain
// ground through the full page budget before failing with "still not empty", which
// names the wrong problem.
//
// A body we did not finish reading cannot be counted. Every key is reported as a
// survivor so the caller's next LIST sees what is genuinely there and the stall
// detector trips on the round that actually stalled.
func TestDeleteObjectsWillNotCountATruncatedBody(t *testing.T) {
	prev := s3PostWithBody
	t.Cleanup(func() { s3PostWithBody = prev })

	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunks/%04d", i)
	}
	// WELL-FORMED AND TRUNCATED, deliberately, and this is the whole point of the
	// flag. A cut usually also breaks the XML, and the parse guard below would catch
	// that — but "the parser happened to choke" is luck, not a rule. The prefix that
	// closes cleanly is the case where counting what arrived looks perfectly valid
	// and is wrong by 920 objects. The flag is what makes the answer not depend on
	// where the cut landed.
	var b strings.Builder
	b.WriteString("<DeleteResult>")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "<Error><Code>InternalError</Code><Key>chunks/%04d</Key></Error>", i)
	}
	b.WriteString("</DeleteResult>")
	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, bool, error) {
		return 200, b.String(), true, nil
	}
	survived, err := s3DeleteObjects("ak", "sk", "ep", "b", keys)
	if err != nil {
		t.Fatalf("a truncated body is not a transport failure: %v", err)
	}
	if survived != len(keys) {
		t.Errorf("survivors = %d, want %d — counting the 80 <Error> elements that FIT reports ~920 "+
			"objects deleted that are still in the bucket, and hides the stall from the caller",
			survived, len(keys))
	}
}

// TestDeleteObjectsWillNotCountAnUnparseableBody. "" and a body that does not
// parse are not evidence that every key was deleted, and substring-counting
// `<Error>` could not tell them from a clean reply.
func TestDeleteObjectsWillNotCountAnUnparseableBody(t *testing.T) {
	prev := s3PostWithBody
	t.Cleanup(func() { s3PostWithBody = prev })
	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, bool, error) {
		return 200, "<DeleteResult><Error><Key>a</Key>", false, nil // cut off, well within the cap
	}
	survived, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if survived != 3 {
		t.Errorf("survivors = %d, want 3 — a body this cannot parse must not be read as a clean delete", survived)
	}
}

// ── from the code review of this PR ─────────────────────────────────────────

// TestReadBoundedBodyDetectsMoreThanItKept. Every test in this package stubs
// s3PostWithBody, so the truncation flag's own derivation had no coverage: drop
// the `+1` from the LimitReader and `truncated` is permanently false, the guard
// that reads it becomes dead code, and the suite stays green. Reading one byte
// PAST the limit is the whole mechanism — io.LimitReader returns a short read
// with NO error, so without it a truncated body is indistinguishable from a
// complete one.
func TestReadBoundedBodyDetectsMoreThanItKept(t *testing.T) {
	for name, tc := range map[string]struct {
		size    int
		wantLen int
		wantTr  bool
	}{
		"well under the cap": {1024, 1024, false},
		"exactly at the cap": {s3ResponseReadLimit, s3ResponseReadLimit, false},
		"one byte over":      {s3ResponseReadLimit + 1, s3ResponseReadLimit, true},
		"far over":           {s3ResponseReadLimit * 2, s3ResponseReadLimit, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, truncated := readBoundedBody(strings.NewReader(strings.Repeat("x", tc.size)))
			if len(got) != tc.wantLen || truncated != tc.wantTr {
				t.Errorf("readBoundedBody(%d bytes) = (%d bytes, truncated=%v), want (%d, %v)",
					tc.size, len(got), truncated, tc.wantLen, tc.wantTr)
			}
		})
	}
}

// TestDeleteObjectsTreatsAnEmptyBodyAsSuccess. Quiet mode reports only FAILURES,
// so a 2xx with nothing in it means every key went — and xml.Unmarshal returns
// io.EOF for it, which the unparseable catch-all graded as "every key survived".
// On a multi-page bucket that stalls the drain for drainMaxStalledRounds and then
// fails, while the deletes were succeeding the whole time.
func TestDeleteObjectsTreatsAnEmptyBodyAsSuccess(t *testing.T) {
	prev := s3PostWithBody
	t.Cleanup(func() { s3PostWithBody = prev })
	for _, body := range []string{"", "\n", "   \n\t"} {
		s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, bool, error) {
			return 200, body, false, nil
		}
		survived, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"a", "b"})
		if err != nil || survived != 0 {
			t.Errorf("an empty 2xx body means nothing failed; got (%d, %v) for %q", survived, err, body)
		}
	}
}
