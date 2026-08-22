package credrotate

// graceperiod_test.go — the window must protect the credential that was live.
//
// The regression is one sentence: a drain that keeps the newest sibling and
// revokes anything older than GRACE_DAYS revokes the PREVIOUSLY-LIVE credential
// every single time, because that credential's age is the ROTATION cadence and
// the cadence is always longer than the window. The window only ever protected
// orphans from a failed run.
//
// Two callers held their own copy of the rule and agreed with each other, which
// is why neither test caught it. So there are two kinds of assertion here: the
// rule, and the AGREEMENT — both real drain functions fed one fixture, so a
// future edit to either has to move both or fail.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

const day = linode.DaySecs

// THE REGRESSION, AS ARITHMETIC. Rotation cadence 60 days, window 7 days: the
// outgoing credential is 60 days old and was superseded a moment ago.
func TestTheOutgoingCredentialIsNeverRevokedOnTheRunThatReplacesIt(t *testing.T) {
	now := int64(1_800_000_000)
	for _, cadenceDays := range []int64{7, 30, 60, 90, 365} {
		t.Run(fmt.Sprintf("cadence %dd", cadenceDays), func(t *testing.T) {
			sorted := []gracedToken{
				{ID: 2, Created: now},                   // just minted
				{ID: 1, Created: now - cadenceDays*day}, // the one it replaces
			}
			drain, inGrace := splitByGrace(sorted, 7, now)
			if len(drain) != 0 {
				t.Errorf("cadence %dd: revoked %v seconds after publishing its replacement — "+
					"any consumer that resolved the credential before this run is now holding a dead one",
					cadenceDays, drain)
			}
			if len(inGrace) != 1 || inGrace[0] != 1 {
				t.Errorf("in-grace = %v, want [1]", inGrace)
			}
		})
	}
}

// AND IT IS REVOKED ONCE THE WINDOW HAS ACTUALLY ELAPSED, or the fix is just a
// leak: superseded credentials would accumulate against the 100-token account
// cap, which is the failure mode on the other side of this decision.
func TestASupersededCredentialDrainsOnceTheWindowElapses(t *testing.T) {
	now := int64(1_800_000_000)
	for _, tc := range []struct {
		name              string
		supersededDaysAgo int64
		wantDrain         bool
	}{
		{"superseded an hour ago", 0, false},
		{"superseded 6 days ago", 6, false},
		{"superseded exactly 7 days ago", 7, true}, // the boundary is inclusive of drain
		{"superseded 8 days ago", 8, true},
		{"superseded 60 days ago", 60, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sorted := []gracedToken{
				{ID: 3, Created: now - tc.supersededDaysAgo*day}, // live; supersedes 2
				{ID: 2, Created: now - 400*day},                  // ancient, and irrelevant
			}
			drain, _ := splitByGrace(sorted, 7, now)
			if got := len(drain) == 1; got != tc.wantDrain {
				t.Errorf("drain = %v, want drained=%v (the subject's own 400-day age must not participate)",
					drain, tc.wantDrain)
			}
		})
	}
}

// THE LIVE ONE IS NEVER JUDGED, whatever its age — a rotation that revoked the
// credential it just wrote everywhere is the worst outcome available here.
func TestTheNewestSiblingIsNeverDrained(t *testing.T) {
	now := int64(1_800_000_000)
	drain, inGrace := splitByGrace([]gracedToken{{ID: 9, Created: now - 3650*day}}, 0, now)
	if len(drain) != 0 || len(inGrace) != 0 {
		t.Errorf("a lone credential produced drain=%v inGrace=%v; it is the live one and gets no decision",
			drain, inGrace)
	}
	// Non-nil so the JSON record carries [] rather than null.
	if drain == nil || inGrace == nil {
		t.Error("both slices must be non-nil for the audit record")
	}
}

// ── the agreement ────────────────────────────────────────────────────────────

// BOTH DRAINS, ONE FIXTURE. pat.go's RunPATRevokeOld (which the in-cluster
// monthly rotation also calls) and broadpat.go's RevokeOldBroadPATs are separate
// functions over the same question. They each had their own copy of the window
// and were identically wrong; nothing compared them. This does.
func TestBothDrainImplementationsAgreeOnTheSameSiblings(t *testing.T) {
	// The REAL clock, because RunPATRevokeOld reads time.Now() internally while
	// RevokeOldBroadPATs takes `now` as a parameter. Handing them a fixed epoch
	// makes one of them see the whole fixture in the future, which is a way for
	// the comparison to pass by both doing nothing.
	now := time.Now().UTC()
	// A ladder: 4 is live, 3 was superseded a moment ago, 2 ten days ago, 1 a
	// hundred. Ages alone would drain 1, 2 AND 3.
	entries := []map[string]any{
		{"id": jn(1), "label": "L", "created": linode.FmtLinodeTS(now.Unix() - 300*day)},
		{"id": jn(2), "label": "L", "created": linode.FmtLinodeTS(now.Unix() - 100*day)},
		{"id": jn(3), "label": "L", "created": linode.FmtLinodeTS(now.Unix() - 10*day)},
		{"id": jn(4), "label": "L", "created": linode.FmtLinodeTS(now.Unix())},
		{"id": jn(9), "label": "OTHER", "created": linode.FmtLinodeTS(now.Unix() - 300*day)},
	}

	broad := &recordingBroadLinode{pats: entries}
	revoked, _ := RevokeOldBroadPATs(context.Background(), broad, "L", 7, now)

	pat := &fakeRotatorClient{listResp: entries}
	captureFirewallOutput(t, func() {
		if err := RunPATRevokeOld(context.Background(), pat, true, "L", 7); err != nil {
			t.Fatal(err)
		}
	})

	if !sameIDs(revoked, pat.deletedIDs) {
		t.Fatalf("the two drains disagree on one fixture: broad-pat revoked %v, credentials-pat revoked %v — "+
			"they are the same rule and must not be able to diverge", revoked, pat.deletedIDs)
	}
	// And they agree on the RIGHT answer, not merely with each other: 3 is inside
	// the window, 2 and 1 are past it, 4 is live, 9 is another family.
	if !sameIDs(revoked, []uint64{1, 2}) {
		t.Errorf("revoked %v, want [1 2]: 4 is live, 3 was superseded by 4 a moment ago, "+
			"2 was superseded 10 days ago and 1 a hundred", revoked)
	}
}

// AN UNREADABLE `created` IS EXCLUDED, NOT TREATED AS ANCIENT. ParseTS answers 0
// for a timestamp it cannot read, and 0 is the epoch — older than every cutoff,
// so a dropped ok flag turned "cannot tell" into "revoke". If the just-minted
// credential is the one that fails to parse it also loses its place at the head
// of the list, and the token already published everywhere is the one deleted.
func TestAnUnreadableCreatedTimeExcludesACredentialFromTheDrain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	broad := &recordingBroadLinode{pats: []map[string]any{
		{"id": jn(1), "label": "L", "created": "not-a-timestamp"},
		{"id": jn(2), "label": "L", "created": linode.FmtLinodeTS(now.Unix() - 100*day)},
		// The live one, minted 10 days ago, so id 2 is genuinely past the window
		// and the test can tell "excluded" from "nothing was drainable anyway".
		{"id": jn(3), "label": "L", "created": linode.FmtLinodeTS(now.Unix() - 10*day)},
	}}
	revoked, _ := RevokeOldBroadPATs(context.Background(), broad, "L", 7, now)
	for _, id := range revoked {
		if id == 1 {
			t.Fatal("a PAT whose `created` could not be read was revoked — 'cannot tell' is not 'old'")
		}
	}
	if !sameIDs(revoked, []uint64{2}) {
		t.Errorf("revoked = %v, want [2]", revoked)
	}
}
