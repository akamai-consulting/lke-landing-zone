package teardown

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
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
	const wf = "../../../../instance-template/.github/workflows/llz-terraform.yml"
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
	oList, oDel, oSleep := objenc.SampleObjectKeys, s3DeleteObjects, drainSleep
	t.Cleanup(func() { objenc.SampleObjectKeys, s3DeleteObjects, drainSleep = oList, oDel, oSleep })
	drainSleep = func(time.Duration) {}
	rounds := 0
	objenc.SampleObjectKeys = func(_, _, _, _ string, _ int) ([]objenc.ObjectRef, error) {
		out := make([]objenc.ObjectRef, 0, *remaining)
		for i := 0; i < *remaining; i++ {
			out = append(out, objenc.ObjectRef{Key: fmt.Sprintf("k%d", i)})
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
	oList, oDel, oSleep := objenc.SampleObjectKeys, s3DeleteObjects, drainSleep
	t.Cleanup(func() { objenc.SampleObjectKeys, s3DeleteObjects, drainSleep = oList, oDel, oSleep })
	drainSleep = func(time.Duration) {}
	objenc.SampleObjectKeys = func(_, _, _, _ string, _ int) ([]objenc.ObjectRef, error) {
		out := make([]objenc.ObjectRef, 0, remaining)
		for i := 0; i < remaining; i++ {
			out = append(out, objenc.ObjectRef{Key: fmt.Sprintf("k%d", i)})
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

	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, error) {
		return 200, `<DeleteResult><Error><Code>InternalError</Code><Key>admins/abc</Key></Error></DeleteResult>`, nil
	}
	survived, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"admins/abc", "other"})
	if err != nil {
		t.Fatalf("a transient per-key InternalError must not fail the batch: %v", err)
	}
	if survived != 1 {
		t.Errorf("survivors = %d, want 1 so the caller retries exactly what is left", survived)
	}

	// A clean response reports nothing surviving.
	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, error) {
		return 200, `<DeleteResult></DeleteResult>`, nil
	}
	if survived, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"k"}); err != nil || survived != 0 {
		t.Errorf("clean delete = (%d, %v), want (0, nil)", survived, err)
	}

	// A transport/HTTP failure is still a hard error — that is not per-key.
	s3PostWithBody = func(_, _, _, _, _ string, _ []byte) (int, string, error) {
		return 503, "upstream unavailable", nil
	}
	if _, err := s3DeleteObjects("ak", "sk", "ep", "b", []string{"k"}); err == nil {
		t.Error("an HTTP 503 must fail the batch rather than read as zero survivors")
	}
}
