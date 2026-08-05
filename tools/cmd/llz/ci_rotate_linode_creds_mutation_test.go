package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// "Nothing to drain" is nil, not a zero-length slice. len(ids) == keepNewest is
// squarely the nothing-to-drain case; `len(ids) < keepNewest` instead returns
// ids[keepNewest:] — an empty but NON-nil slice, which the existing table test
// (which compares lengths) accepts.
func TestIdsToDrainReturnsNilWhenNothingToDrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []uint64
		keep int
	}{
		{"exactly keepNewest", []uint64{5, 3}, 2},
		{"fewer than keepNewest", []uint64{5}, 2},
		{"single id, keep floored at 1", []uint64{42}, 0},
	} {
		if got := idsToDrain(append([]uint64(nil), tc.ids...), tc.keep); got != nil {
			t.Errorf("%s: idsToDrain = %#v, want nil", tc.name, got)
		}
	}
}

// Object-storage rotation without OBJ_CLUSTER cannot mint a replacement key, so
// the rotator must refuse UP FRONT — before it touches the Linode API. Only the
// OBJ_CLUSTER-present path was covered, so the guard could be inverted into
// "refuse when it IS set" unnoticed.
func TestRunRotateLinodeCredsRefusesWithoutObjCluster(t *testing.T) {
	// The prefix gate comes first now; this test is about the OBJ_CLUSTER one.
	t.Setenv("OBJ_LABEL_PREFIX", "acme")
	t.Setenv("REGION", "primary")
	t.Setenv("OBJ_CLUSTER", "")
	t.Setenv("LINODE_TOKEN", "minting")
	lc := &stubLinode{}
	bao := &stubBao{data: map[string]map[string]string{}}
	withRotatorStubs(t, lc, bao, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))

	err := runRotateLinodeCreds(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "OBJ_CLUSTER must be set") {
		t.Fatalf("err = %v, want an OBJ_CLUSTER refusal", err)
	}
	if lc.objCreates != 0 || len(bao.data) != 0 {
		t.Errorf("must refuse before minting/writing anything: objCreates=%d baoPaths=%d", lc.objCreates, len(bao.data))
	}
}

// failingDrain revokes nothing successfully — the drain half is best-effort, so
// the warning log is the only observable of a failed revoke.
type failingDrain struct{ *stubLinode }

func (f failingDrain) DeleteObjectStorageKey(context.Context, uint64) error {
	return errors.New("403 unauthorized")
}

func TestDrainOldLogsFailedRevokesAndStaysQuietOnSuccess(t *testing.T) {
	var objEntry credEntry
	for _, e := range buildRotationTable("acme", "primary", "us-ord-1") {
		if e.kind == credKindObjKey {
			objEntry = e
			break
		}
	}
	if objEntry.label == "" {
		t.Fatal("no object-storage entry in the rotation table")
	}
	keys := []map[string]any{
		{"id": jn(10), "label": objEntry.label},
		{"id": jn(11), "label": objEntry.label},
		{"id": jn(12), "label": objEntry.label},
	}

	t.Run("success is silent", func(t *testing.T) {
		lc := &stubLinode{objkeys: keys}
		out := captureStderr(t, func() { drainOld(context.Background(), lc, objEntry, 1) })
		if len(lc.deleted) != 2 {
			t.Fatalf("deleted = %v, want the two oldest ids", lc.deleted)
		}
		if out != "" {
			t.Errorf("successful revokes must log nothing, got %q", out)
		}
	})

	t.Run("failure is warned about", func(t *testing.T) {
		lc := failingDrain{&stubLinode{objkeys: keys}}
		out := captureStderr(t, func() { drainOld(context.Background(), lc, objEntry, 1) })
		if !strings.Contains(out, "revoke id=") || !strings.Contains(out, "403 unauthorized") {
			t.Errorf("a failed revoke must warn with the id and cause, got %q", out)
		}
	})
}
