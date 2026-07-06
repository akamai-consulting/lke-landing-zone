package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// fakeEnvWriter records each GitHub environment-secret publish.
type fakeEnvWriter struct {
	calls []string // "name@env=value"
	err   error
}

func (f *fakeEnvWriter) write(name, env, value string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, name+"@"+env+"="+value)
	return nil
}

func broadDeps(lc rotatorLinodeAPI, bao baoStore, w envSecretWriter, now time.Time) broadPATDeps {
	return broadPATDeps{lc: lc, bao: bao, writeSecret: w, now: func() time.Time { return now }}
}

func TestRotateBroadPAT_NotDue(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	// rotated 10 days ago, threshold 60 → not due.
	bao := &stubBao{data: map[string]map[string]string{
		broadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -10).Unix())},
	}}
	lc := &stubLinode{}
	w := &fakeEnvWriter{}
	withRotatorStubs(t, lc, bao, now)
	rec, err := rotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		broadPATOpts{label: "L", deployments: []string{"primary"}, rotateAfter: 60, graceDays: 7, apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec["action"] != "skip" {
		t.Errorf("want skip, got %v", rec["action"])
	}
	if lc.patCreates != 0 || len(w.calls) != 0 || len(lc.deleted) != 0 {
		t.Errorf("not-due must mint/publish/revoke nothing: creates=%d pub=%v del=%v", lc.patCreates, w.calls, lc.deleted)
	}
}

func TestRotateBroadPAT_DryRun(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	bao := &stubBao{data: map[string]map[string]string{broadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -90).Unix())}}}
	lc := &stubLinode{}
	w := &fakeEnvWriter{}
	withRotatorStubs(t, lc, bao, now)
	rec, _ := rotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		broadPATOpts{label: "L", deployments: []string{"primary"}, rotateAfter: 60, apply: false})
	if rec["action"] != "would-rotate" {
		t.Errorf("want would-rotate, got %v", rec["action"])
	}
	if lc.patCreates != 0 || len(w.calls) != 0 || len(lc.deleted) != 0 {
		t.Errorf("dry-run must change nothing: creates=%d pub=%v del=%v", lc.patCreates, w.calls, lc.deleted)
	}
}

func TestRotateBroadPAT_ApplyFullFlow(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	bao := &stubBao{data: map[string]map[string]string{broadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -90).Unix())}}}
	// Two OLD siblings (past grace) to revoke + one recent (in grace) to keep.
	old1 := now.AddDate(0, 0, -80)
	old2 := now.AddDate(0, 0, -70)
	recent := now.AddDate(0, 0, -2) // within 7d grace
	lc := &stubLinode{pats: []map[string]any{
		{"id": jn(11), "label": "L", "created": linode.FmtLinodeTS(old1.Unix())},
		{"id": jn(12), "label": "L", "created": linode.FmtLinodeTS(old2.Unix())},
		{"id": jn(13), "label": "L", "created": linode.FmtLinodeTS(recent.Unix())},
		{"id": jn(99), "label": "OTHER", "created": linode.FmtLinodeTS(old1.Unix())}, // different label — untouched
	}}
	w := &fakeEnvWriter{}
	withRotatorStubs(t, lc, bao, now)
	rec, err := rotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		broadPATOpts{label: "L", deployments: []string{"primary", "secondary"}, rotateAfter: 60, graceDays: 7, apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec["action"] != "rotated" {
		t.Fatalf("want rotated, got %v", rec["action"])
	}
	// Minted exactly one successor.
	if lc.patCreates != 1 {
		t.Errorf("want 1 mint, got %d", lc.patCreates)
	}
	// OpenBao got the new token + a fresh rotated_at.
	if bao.data[broadPATBaoPath]["token"] != "new-pat" || bao.data[broadPATBaoPath]["rotated_at"] != itoa(now.Unix()) {
		t.Errorf("OpenBao not updated: %v", bao.data[broadPATBaoPath])
	}
	// Published to BOTH deployments' env secrets, with the new token.
	if len(w.calls) != 2 ||
		w.calls[0] != "LINODE_API_TOKEN@infra-primary=new-pat" ||
		w.calls[1] != "LINODE_API_TOKEN@infra-secondary=new-pat" {
		t.Errorf("env publish wrong: %v", w.calls)
	}
	// Revoked the two OLD same-labeled siblings (11, 12); kept the in-grace 13 + the
	// different-label 99.
	if !sameIDs(lc.deleted, []uint64{11, 12}) {
		t.Errorf("revoked wrong ids: %v (want 11,12)", lc.deleted)
	}
}

func TestRotateBroadPAT_VerifyFailAbortsBeforeAnyWrite(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	bao := &stubBao{data: map[string]map[string]string{broadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -90).Unix())}}}
	lc := &stubLinode{verifyErr: context.DeadlineExceeded, pats: []map[string]any{
		{"id": jn(11), "label": "L", "created": linode.FmtLinodeTS(now.AddDate(0, 0, -80).Unix())},
	}}
	w := &fakeEnvWriter{}
	withRotatorStubs(t, lc, bao, now)
	_, err := rotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		broadPATOpts{label: "L", deployments: []string{"primary"}, rotateAfter: 60, graceDays: 7, apply: true})
	if err == nil {
		t.Fatal("verify failure must error")
	}
	// A bad mint must publish nothing, write nothing to OpenBao, revoke nothing.
	if _, ok := bao.data[broadPATBaoPath]["token"]; ok {
		t.Error("OpenBao must not be written on verify failure")
	}
	if len(w.calls) != 0 || len(lc.deleted) != 0 {
		t.Errorf("verify failure must not publish/revoke: pub=%v del=%v", w.calls, lc.deleted)
	}
}

func TestRotateBroadPAT_PublishFailSkipsRevoke(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	bao := &stubBao{data: map[string]map[string]string{broadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -90).Unix())}}}
	lc := &stubLinode{pats: []map[string]any{
		{"id": jn(11), "label": "L", "created": linode.FmtLinodeTS(now.AddDate(0, 0, -80).Unix())},
	}}
	w := &fakeEnvWriter{err: context.DeadlineExceeded} // publish fails
	withRotatorStubs(t, lc, bao, now)
	_, err := rotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		broadPATOpts{label: "L", deployments: []string{"primary"}, rotateAfter: 60, graceDays: 7, apply: true})
	if err == nil {
		t.Fatal("publish failure must error")
	}
	// The new token is minted + in OpenBao, but the OLD PAT is NOT revoked (still
	// valid for CI until a later successful run) — the key safety property.
	if len(lc.deleted) != 0 {
		t.Errorf("publish failure must NOT revoke the old PAT (it's still in use): %v", lc.deleted)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func sameIDs(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[uint64]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, wnt := range want {
		if seen[wnt] == 0 {
			return false
		}
		seen[wnt]--
	}
	return true
}
