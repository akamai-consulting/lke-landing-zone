package credrotate

// Mutation-test gap closure for ci_rotate_broad_pat.go. Everything here pins a
// decision that decides WHICH Linode PAT is minted, kept or revoked — the places
// where a silent off-by-one turns "drain the old token" into "revoke the token CI
// is holding right now".

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// recordingBroadLinode is stubLinode with the two things the revoke/mint
// decisions turn on and the shared stub drops: the exact arguments the mint was
// called with, and a per-id delete failure. The freshly minted PAT is PREPENDED
// to the account listing, mirroring the real flow where the successor shows up
// in the very next ListProfileTokens.
type recordingBroadLinode struct {
	pats       []map[string]any
	deleted    []uint64
	deleteErrs map[uint64]error
	newID      int
	created    string // `created` stamp of the minted PAT as it appears in the listing

	label, scopes, expiry string
	patCreates            int
}

func (s *recordingBroadLinode) ListProfileTokens(context.Context) ([]map[string]any, error) {
	return s.pats, nil
}

func (s *recordingBroadLinode) CreateProfileToken(_ context.Context, label, scopes, expiry string) (map[string]any, error) {
	s.patCreates++
	s.label, s.scopes, s.expiry = label, scopes, expiry
	if s.created != "" {
		s.pats = append([]map[string]any{{"id": jn(s.newID), "label": label, "created": s.created}}, s.pats...)
	}
	return map[string]any{"id": jn(s.newID), "token": "new-pat"}, nil
}

func (s *recordingBroadLinode) DeleteProfileToken(_ context.Context, id uint64) error {
	if err := s.deleteErrs[id]; err != nil {
		return err
	}
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *recordingBroadLinode) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return nil, nil
}

func (s *recordingBroadLinode) CreateObjectStorageKeyBuckets(context.Context, string, string, []string, string) (map[string]any, error) {
	return nil, errors.New("not used")
}
func (s *recordingBroadLinode) DeleteObjectStorageKey(context.Context, uint64) error { return nil }
func (s *recordingBroadLinode) Verify(context.Context) error                         { return nil }

// The successor must be minted with a 90-DAY expiry computed forward from now.
// Nothing else in the suite looked at the expiry argument, so an inverted or
// mis-scaled validity window (a PAT that expires in the past, or in 90 seconds)
// would have shipped silently and killed every workflow at the next rotation.
func TestRotateBroadPATMintsWithNinetyDayExpiry(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	bao := &stubBao{data: map[string]map[string]string{
		BroadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -90).Unix())},
	}}
	lc := &recordingBroadLinode{newID: 42}
	w := &fakeEnvWriter{}
	withRotatorStubs(t, lc, bao, now)

	if _, err := RotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		BroadPATOpts{label: "L", deployments: []string{"primary"}, rotateAfter: 60, graceDays: 7, apply: true}); err != nil {
		t.Fatal(err)
	}
	if want := linode.FmtLinodeTS(now.Unix() + BroadPATValidityDays*linode.DaySecs); lc.expiry != want {
		t.Errorf("mint expiry = %q, want %q (now + %dd)", lc.expiry, want, BroadPATValidityDays)
	}
	// Belt-and-braces on the direction + magnitude, independent of the formatter.
	secs, ok := linode.ParseTS(lc.expiry)
	if !ok {
		t.Fatalf("expiry %q is not a Linode timestamp", lc.expiry)
	}
	if got := secs - now.Unix(); got != BroadPATValidityDays*linode.DaySecs {
		t.Errorf("expiry is %d seconds out, want %d (a past or near-instant expiry breaks CI at the next run)",
			got, BroadPATValidityDays*linode.DaySecs)
	}
	if lc.label != "L" || lc.scopes != BroadPATScopes {
		t.Errorf("mint called with label=%q scopes=%q", lc.label, lc.scopes)
	}
}

// The newest-first sort is what keeps cands[0] — the PAT this run just minted —
// out of the revoke loop. Linode `created` stamps are second-granularity, so a
// tie between the successor and a sibling is constructible; on a tie the sort
// must NOT reorder the just-minted token out of the protected slot. With a zero
// grace window nothing else stands between it and DeleteProfileToken.
func TestRotateBroadPATNeverRevokesTheJustMintedTokenOnACreatedTie(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	stamp := linode.FmtLinodeTS(now.Unix())
	bao := &stubBao{data: map[string]map[string]string{
		BroadPATBaoPath: {"rotated_at": itoa(now.AddDate(0, 0, -90).Unix())},
	}}
	lc := &recordingBroadLinode{
		newID:   42,
		created: stamp, // the successor lands with created == now …
		pats: []map[string]any{
			{"id": jn(11), "label": "L", "created": stamp}, // … and so did this sibling
		},
	}
	w := &fakeEnvWriter{}
	withRotatorStubs(t, lc, bao, now)

	rec, err := RotateBroadPAT(context.Background(), broadDeps(lc, bao, w.write, now),
		BroadPATOpts{label: "L", deployments: []string{"primary"}, rotateAfter: 60, graceDays: 0, apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec["action"] != "rotated" {
		t.Fatalf("want rotated, got %v", rec["action"])
	}
	for _, id := range lc.deleted {
		if id == 42 {
			t.Fatalf("the just-minted PAT (id=42) was revoked — the rotation destroyed its own successor: deleted=%v", lc.deleted)
		}
	}
	if !sameIDs(lc.deleted, []uint64{11}) {
		t.Errorf("deleted = %v, want only the old sibling 11", lc.deleted)
	}
}

// The grace window is the only thing keeping a token a running workflow may
// still be holding from being pulled out from under it. Pin BOTH edges: a PAT
// created exactly ON the cutoff is outside the window (revoked), one second
// newer is inside it (skipped).
func TestRevokeOldBroadPATsGraceWindowEdges(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	const graceDays int64 = 7
	cutoff := now.Unix() - graceDays*linode.DaySecs

	lc := &recordingBroadLinode{pats: []map[string]any{
		{"id": jn(1), "label": "L", "created": linode.FmtLinodeTS(now.Unix())},      // newest — never revoked
		{"id": jn(2), "label": "L", "created": linode.FmtLinodeTS(cutoff + 1)},      // just inside grace
		{"id": jn(3), "label": "L", "created": linode.FmtLinodeTS(cutoff)},          // exactly ON the cutoff → outside
		{"id": jn(4), "label": "L", "created": linode.FmtLinodeTS(cutoff - 86_400)}, // well past
		{"id": jn(9), "label": "OTHER", "created": linode.FmtLinodeTS(cutoff - 1)},  // another family entirely
	}}

	revoked, skipped := RevokeOldBroadPATs(context.Background(), lc, "L", graceDays, now)
	if !sameIDs(revoked, []uint64{3, 4}) {
		t.Errorf("revoked = %v, want [3 4] (a PAT created exactly on the cutoff is outside the window)", revoked)
	}
	if !sameIDs(skipped, []uint64{2}) {
		t.Errorf("skipped-in-grace = %v, want [2]", skipped)
	}
	if !sameIDs(lc.deleted, []uint64{3, 4}) {
		t.Errorf("DeleteProfileToken calls = %v, want [3 4]", lc.deleted)
	}
}

// A revoke that FAILED must not be reported as revoked. The record is the audit
// trail for "this credential is dead"; claiming a still-live PAT was drained
// hides it from every subsequent inventory.
func TestRevokeOldBroadPATsFailedDeleteIsNotReportedRevoked(t *testing.T) {
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	old := linode.FmtLinodeTS(now.Unix() - 80*linode.DaySecs)
	lc := &recordingBroadLinode{
		deleteErrs: map[uint64]error{11: errors.New("linode 500")},
		pats: []map[string]any{
			{"id": jn(1), "label": "L", "created": linode.FmtLinodeTS(now.Unix())},
			{"id": jn(11), "label": "L", "created": old}, // delete fails
			{"id": jn(12), "label": "L", "created": old}, // delete succeeds
		},
	}
	revoked, _ := RevokeOldBroadPATs(context.Background(), lc, "L", 7, now)
	if !sameIDs(revoked, []uint64{12}) {
		t.Errorf("revoked = %v, want [12] only — a failed DeleteProfileToken must never be recorded as a revocation", revoked)
	}
}
