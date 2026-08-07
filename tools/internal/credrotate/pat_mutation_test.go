package credrotate

// Boundary/arithmetic tests for credentials_pat.go — the PAT lifecycle numbers
// that decide whether a minted token expires when we think it does and whether
// the reaper revokes a credential that is still in use. Mutation testing found
// the existing suite blind to off-by-one boundaries (validity-days >= 1,
// grace-days >= 0, the grace-window comparison) and to the expiry/cutoff
// arithmetic itself, because every case used round numbers or year-2099
// timestamps that survive a sign flip.
//
// RunPATCreate/RevokeOld read time.Now() directly (no clock seam), so
// these tests bracket the real clock instead of freezing it: capture now before
// and after the call and assert the computed timestamp lands inside the window.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// patTS renders an epoch second as the Linode timestamp the list API returns.
func patTS(unix int64) string { return linode.FmtLinodeTS(unix) }

// captureSlog swaps the default slog logger for a JSON handler writing into a
// buffer (same format credentialsCmd installs) and returns the decoded records.
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(orig)
	fn()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("slog line is not JSON: %v\n%s", err, line)
		}
		recs = append(recs, m)
	}
	return recs
}

// ── pat create: validity floor + expiry arithmetic ───────────────────────────

// A one-day validity is legal (the floor is < 1, not <= 1) — the shortest window
// an operator can ask for must still mint a token.
func TestCredentialsPATCreateAcceptsOneDayValidity(t *testing.T) {
	client := &fakeRotatorClient{createResp: map[string]any{"id": json.Number("7"), "token": "tok"}}
	var err error
	captureFirewallOutput(t, func() {
		err = RunPATCreate(context.Background(), client, true, "lbl", "s", 1, "", nil)
	})
	if err != nil {
		t.Fatalf("validity-days=1 must be accepted, got %v", err)
	}
	if client.createdExpiry == "" {
		t.Fatal("validity-days=1 did not reach CreateProfileToken")
	}
}

// The expiry handed to the Linode API must be now + validityDays*DaySecs: a sign
// flip mints an already-expired token, and a *→/ mints one that expires today.
func TestCredentialsPATCreateExpiryIsNowPlusValidityDays(t *testing.T) {
	for _, days := range []int64{1, 30, 90} {
		client := &fakeRotatorClient{createResp: map[string]any{"id": json.Number("7"), "token": "tok"}}
		var err error
		before := time.Now().Unix()
		captureFirewallOutput(t, func() {
			err = RunPATCreate(context.Background(), client, true, "lbl", "s", days, "", nil)
		})
		after := time.Now().Unix()
		if err != nil {
			t.Fatalf("validity-days=%d: %v", days, err)
		}
		got, ok := linode.ParseTS(client.createdExpiry)
		if !ok {
			t.Fatalf("validity-days=%d: expiry %q is not a Linode timestamp", days, client.createdExpiry)
		}
		lo, hi := before+days*linode.DaySecs, after+days*linode.DaySecs
		if got < lo || got > hi {
			t.Errorf("validity-days=%d: expiry=%d (%s), want within [%d,%d] (now+%d days)",
				days, got, client.createdExpiry, lo, hi, days)
		}
	}
}

// The dry-run record's expiry_planned is the same arithmetic on the no-write path.
func TestCredentialsPATCreateDryRunExpiryPlanned(t *testing.T) {
	var err error
	before := time.Now().Unix()
	stdout, _ := captureFirewallOutput(t, func() {
		err = RunPATCreate(context.Background(), &fakeRotatorClient{}, false, "lbl", "s", 90, "", nil)
	})
	after := time.Now().Unix()
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	planned, _ := rec["expiry_planned"].(string)
	got, ok := linode.ParseTS(planned)
	if !ok {
		t.Fatalf("expiry_planned %q is not a Linode timestamp", planned)
	}
	lo, hi := before+90*linode.DaySecs, after+90*linode.DaySecs
	if got < lo || got > hi {
		t.Errorf("expiry_planned=%d (%s), want within [%d,%d]", got, planned, lo, hi)
	}
}

// ── pat revoke-old: grace floor, cutoff arithmetic, window boundary ──────────

// grace-days=0 is legal ("revoke every older sibling now") — only negatives are
// rejected. With no grace the older sibling drains immediately.
func TestCredentialsPATRevokeOldAcceptsZeroGrace(t *testing.T) {
	now := time.Now().Unix()
	client := &fakeRotatorClient{listResp: []map[string]any{
		patListEntry(1, "lbl", patTS(now-10*linode.DaySecs)),
		patListEntry(2, "lbl", patTS(now-linode.DaySecs)),
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = RunPATRevokeOld(context.Background(), client, true, "lbl", 0)
	})
	if err != nil {
		t.Fatalf("grace-days=0 must be accepted, got %v", err)
	}
	rec := decodeRecord(t, stdout)
	if rec["kept_pat_id"] != float64(2) || fmt.Sprint(rec["revoked_ids"]) != "[1]" {
		t.Errorf("grace-days=0: kept=%v revoked=%v, want kept=2 revoked=[1]", rec["kept_pat_id"], rec["revoked_ids"])
	}
}

// The cutoff is now - graceDays*DaySecs. Anchored on the real clock (not the
// year-2099 sentinels the older tests use) so a sign flip or a *→/ moves the
// cutoff across a sibling and changes who gets revoked.
func TestCredentialsPATRevokeOldCutoffIsNowMinusGraceDays(t *testing.T) {
	now := time.Now().Unix()
	client := &fakeRotatorClient{listResp: []map[string]any{
		patListEntry(10, "lbl", patTS(now-30*linode.DaySecs)), // well past the window → revoke
		patListEntry(20, "lbl", patTS(now-3*linode.DaySecs)),  // inside a 7-day window → keep
		patListEntry(30, "lbl", patTS(now-3600)),              // an hour old → the live one
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = RunPATRevokeOld(context.Background(), client, true, "lbl", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["kept_pat_id"] != float64(30) {
		t.Errorf("kept_pat_id = %v, want 30", rec["kept_pat_id"])
	}
	if got := fmt.Sprint(rec["revoked_ids"]); got != "[10]" {
		t.Errorf("revoked_ids = %v, want [10] (3-day-old sibling is inside the 7-day grace window)", rec["revoked_ids"])
	}
	if got := fmt.Sprint(rec["skipped_in_grace_ids"]); got != "[20]" {
		t.Errorf("skipped_in_grace_ids = %v, want [20]", rec["skipped_in_grace_ids"])
	}
	if fmt.Sprint(client.deletedIDs) != "[10]" {
		t.Errorf("deleted %v, want [10]", client.deletedIDs)
	}
}

// A sibling created exactly ON the cutoff is revoked — the comparison is
// `created > cutoff` (strictly inside the window survives), not `>=`. Needs the
// test's `now` and the one the function reads to be the same wall-clock second,
// so we start just after a second tick and re-check afterwards.
func TestCredentialsPATRevokeOldCutoffBoundaryIsExclusive(t *testing.T) {
	for attempt := 0; attempt < 5; attempt++ {
		// Land in the first 200ms of a second, leaving ~800ms of slack.
		for i := 0; i < 2000 && time.Now().Nanosecond() > int(200*time.Millisecond); i++ {
			time.Sleep(time.Millisecond)
		}
		now := time.Now().Unix()
		// grace-days=0 → cutoff == now; id 21 is created exactly at the cutoff.
		client := &fakeRotatorClient{listResp: []map[string]any{
			patListEntry(20, "lbl", patTS(now+3600)), // newest → kept
			patListEntry(21, "lbl", patTS(now)),      // exactly on the cutoff
		}}
		var err error
		stdout, _ := captureFirewallOutput(t, func() {
			err = RunPATRevokeOld(context.Background(), client, true, "lbl", 0)
		})
		if time.Now().Unix() != now {
			continue // the clock rolled into the next second mid-call; retry
		}
		if err != nil {
			t.Fatal(err)
		}
		rec := decodeRecord(t, stdout)
		if got := fmt.Sprint(rec["revoked_ids"]); got != "[21]" {
			t.Errorf("revoked_ids = %v, want [21]: created == cutoff is NOT inside the grace window", rec["revoked_ids"])
		}
		if got := fmt.Sprint(rec["skipped_in_grace_ids"]); got != "[]" {
			t.Errorf("skipped_in_grace_ids = %v, want []", rec["skipped_in_grace_ids"])
		}
		return
	}
	t.Skip("could not run the call inside a single wall-clock second")
}

// Ties in `created` must resolve to the first-listed PAT: the sort comparator is
// a strict `>`, which insertion-sort leaves in input order. A non-strict `>=`
// comparator reverses equal elements and hands the "keep" slot — i.e. which
// credential survives revocation — to the other PAT.
func TestCredentialsPATRevokeOldTieKeepsFirstListed(t *testing.T) {
	ts := patTS(time.Now().Unix())
	client := &fakeRotatorClient{listResp: []map[string]any{
		patListEntry(11, "lbl", ts),
		patListEntry(12, "lbl", ts),
	}}
	var err error
	stdout, _ := captureFirewallOutput(t, func() {
		err = RunPATRevokeOld(context.Background(), client, true, "lbl", 7)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, stdout)
	if rec["kept_pat_id"] != float64(11) {
		t.Errorf("kept_pat_id = %v, want 11 (equal created → first listed wins)", rec["kept_pat_id"])
	}
	if got := fmt.Sprint(rec["skipped_in_grace_ids"]); got != "[12]" {
		t.Errorf("skipped_in_grace_ids = %v, want [12]", rec["skipped_in_grace_ids"])
	}
	if len(client.deletedIDs) != 0 {
		t.Errorf("deleted %v, want none (both inside the grace window)", client.deletedIDs)
	}
}

// The grace-window log line reports the sibling's real age in days — the number
// an operator reads to decide whether the reaper is behaving.
func TestCredentialsPATRevokeOldGraceLogAgeDays(t *testing.T) {
	now := time.Now().Unix()
	client := &fakeRotatorClient{listResp: []map[string]any{
		patListEntry(20, "lbl", patTS(now-3*linode.DaySecs)),
		patListEntry(30, "lbl", patTS(now)),
	}}
	var err error
	recs := captureSlog(t, func() {
		captureFirewallOutput(t, func() {
			err = RunPATRevokeOld(context.Background(), client, true, "lbl", 7)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range recs {
		msg, _ := r["msg"].(string)
		if !strings.Contains(msg, "in grace window") {
			continue
		}
		found = true
		if r["id"] != float64(20) {
			t.Errorf("grace log id = %v, want 20", r["id"])
		}
		if r["age_days"] != float64(3) {
			t.Errorf("grace log age_days = %v, want 3", r["age_days"])
		}
	}
	if !found {
		t.Fatalf("no grace-window log record; got %v", recs)
	}
}
