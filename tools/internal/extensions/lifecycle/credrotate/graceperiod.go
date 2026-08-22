package credrotate

// graceperiod.go — when a superseded credential becomes safe to revoke.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE GRACE WINDOW WAS MEASURED FROM THE WRONG EVENT, in both copies of this
// rule, and the consequence is the opposite of what the window is for.
//
// A drain keeps the newest same-labeled credential (the one just minted) and
// revokes its older siblings. The window exists so a consumer still holding the
// PREVIOUS credential has time to pick up the new one. Both implementations
// asked "is this sibling younger than GRACE_DAYS?" — its own age — and revoked
// it otherwise.
//
// But the previously-live credential is, by construction, about as old as the
// ROTATION cadence. At ROTATE_AFTER_DAYS=60 with GRACE_DAYS=7, the token that
// was live until a moment ago is 60 days old, fails a 7-day age test, and is
// deleted SECONDS after its replacement is published. The window only ever
// protected orphans from a failed run — never the live token, which is exactly
// what both call sites' comments claim it protects.
//
// What that cost, per caller:
//
//   - the broad account PAT: any llz-terraform.yml apply in flight resolved
//     LINODE_API_TOKEN at job start and starts 401ing mid-apply.
//   - the in-cluster PAT: revoked seconds after the KV write, while ESO (5m
//     refresh) and kubelet (~1m projection) still serve the old value — up to
//     ~6 minutes in which the volume-labeler, cidr-firewall, the DNS-01 solver
//     webhook and ExternalDNS all hold a revoked token.
//
// THE RIGHT EVENT IS SUPERSESSION, and it needs no new state to find. Sort the
// siblings newest-first and a credential was superseded when the NEXT-NEWER one
// was minted — so `sorted[i]`'s creation time is when `sorted[i+1]` stopped
// being the live one. The window is measured from there.
//
// ONE FUNCTION BECAUSE THERE WERE TWO, AND THEY AGREED. pat.go's
// RunPATRevokeOld (which the in-cluster monthly rotation also calls) and
// broadpat.go's RevokeOldBroadPATs each carried their own copy, identical and
// identically wrong. objkey.go does not participate: the OBJ-keys API exposes no
// created time at all, so that lifecycle drains by keep-newest-N instead, which
// is a different rule and deliberately stays separate.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// gracedToken is one same-labeled credential: its id and when it was minted.
type gracedToken struct {
	ID      uint64
	Created int64 // unix seconds, from the Linode API's `created`
}

// graceDecision is what the window decided about one superseded credential, and
// WHEN it was superseded — which is the number an operator needs and the one
// neither caller could print while both were measuring the wrong thing. A log
// line reading "age_days=60" on a token that went out of service ten seconds
// ago is worse than no log line: it is the wrong clock, stated confidently.
type graceDecision struct {
	ID           uint64
	SupersededAt int64
	Drain        bool
}

// decideByGrace judges every superseded sibling against the overlap window.
//
// sorted MUST be newest-first; sorted[0] is the live credential and gets no
// decision — it is never revoked, whatever its age.
func decideByGrace(sorted []gracedToken, graceDays, now int64) []graceDecision {
	if len(sorted) < 2 {
		return nil
	}
	cutoff := now - graceDays*linode.DaySecs
	out := make([]graceDecision, 0, len(sorted)-1)
	for i, c := range sorted[1:] {
		// sorted[i] is the sibling one position NEWER than c, so its creation is
		// the moment c stopped being live. Reading c.Created here instead is the
		// defect this file exists to describe.
		supersededAt := sorted[i].Created
		out = append(out, graceDecision{
			ID:           c.ID,
			SupersededAt: supersededAt,
			Drain:        supersededAt <= cutoff,
		})
	}
	return out
}

// splitByGrace is decideByGrace reduced to the two id lists both callers put
// into their JSON record. Non-nil slices so an empty list marshals as [] rather
// than null.
func splitByGrace(sorted []gracedToken, graceDays, now int64) (drain, inGrace []uint64) {
	drain, inGrace = []uint64{}, []uint64{}
	for _, d := range decideByGrace(sorted, graceDays, now) {
		if d.Drain {
			drain = append(drain, d.ID)
		} else {
			inGrace = append(inGrace, d.ID)
		}
	}
	return drain, inGrace
}
