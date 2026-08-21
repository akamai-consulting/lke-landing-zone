package linode

// lke_versions.go — the LKE-Enterprise version catalog, so `llz doctor` can ask
// the ACCOUNT whether the k8sVersion an instance pins is actually offered to it,
// instead of finding out inside a terraform apply.
//
// Route: /v4beta/lke/tiers/{tier}/versions. LKE-Enterprise is a v4beta,
// enterprise-tier-only product (see docs/adr/0005-managed-app-platform.md), so
// the standard /v4/lke/versions catalog is the wrong list to check against — it
// answers for a product this landing zone does not use.
//
// THE RESPONSE BODY HAS NOW BEEN SEEN, and it is why this stopped being purely
// advisory. When this file was written only the route's existence was verified (an
// unknown Linode path returns 404 before auth; this one returns 401) and the shape
// of an entitled account's answer was a guess. Measured on 2026-08-11 against two
// accounts in the same hour, it returns FULL LKE-E build ids and nothing coarser:
// one account listed `v1.33.6+lke7`, the e2e account listed exactly
// `v1.34.6+lke2` and `v1.32.9+lke4`.
//
// TWO THINGS FOLLOW, and both are load-bearing. The catalog is per-ACCOUNT, so
// "is this version valid" has no answer except against the account that will build
// the cluster — release notes cannot answer it, and a pin that worked at 16:04 was
// rejected at 17:06. And because the ids are exact, an absent pin is a real
// verdict rather than a spelling difference, which is what lets
// `llz ci assert-k8s-version` FAIL a build on it (see VersionOffered).
//
// What has not changed: a shape this file did not expect is still UNKNOWN. An
// error, an empty list, or entries carrying no build suffix must be reported as
// uncertainty and never as "not offered" — both callers are written to that
// contract.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LKETierEnterprise is the tier segment for LKE-Enterprise.
const LKETierEnterprise = "enterprise"

// LKEVersionsPath is the ONE definition of the route this catalog is read from,
// and it is exported because a SECOND caller now needs the same string for a
// different purpose: `llz ci validate-tokens` probes this exact path to prove the
// pipeline's Linode PAT is authorized for it.
//
// WHY THAT PROBE MUST NOT SPELL THE ROUTE ITSELF. `llz ci assert-k8s-version`
// warns and PASSES when this read is refused — deliberately, because a build must
// not be blocked on a question nobody could ask. The consequence is that the gate
// can be permanently inert while looking green, so the credential probe is the
// thing that measures whether the question is answerable at all (issue #449). A
// probe holding its own copy of the route would keep saying "authorized" about a
// path this function no longer reads the day the route moves — the split-contract
// failure docs/e2e-gates.md is written around, in the one place whose whole job is
// to notice that the real read cannot happen.
//
// TestTheReadsUseTheExportedRouteDefinitions holds the producer side of that to
// this definition; TestTheCatalogProbeAsksTheRouteTheGateActuallyReads holds the
// probe to it.
func LKEVersionsPath(tier string) string {
	return "/v4beta/lke/tiers/" + tier + "/versions"
}

// ListLKEVersions returns the Kubernetes versions the ACCOUNT may create in the
// given LKE tier, newest-first as the API returns them.
//
// Each entry is reported under `id` (the Linode list convention). AN ENTRY THIS
// CANNOT READ IS AN ERROR, NOT A SKIP, and that changed when the list stopped
// being advisory. It used to drop such a row and return the rest, on the reasoning
// that "a partial list is still useful for reporting, and the caller never treats
// a short list as proof of absence" — which was true while the only caller was a
// print. `llz ci assert-k8s-version` treats absence as a verdict and FAILS a build
// on it, so a silently shortened list is a pin reported as unbuildable because
// this function could not parse the row that offered it.
//
// A row with no usable `id` means the response is not the shape this was measured
// against, which is exactly the uncertainty both callers already degrade on. Say
// so and let them.
func (c *Client) ListLKEVersions(ctx context.Context, tier string) ([]string, error) {
	raw, err := c.listAllPages(ctx, LKEVersionsPath(tier))
	if err != nil {
		return nil, err
	}
	return parseLKEVersions(tier, raw)
}

// parseLKEVersions is the pure half, split out so the refusal above is testable
// without a live account — the arm that matters is the one no fixture reaches by
// accident.
func parseLKEVersions(tier string, raw []map[string]any) ([]string, error) {
	out := make([]string, 0, len(raw))
	for i, m := range raw {
		id, ok := m["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("entry %d of the LKE %s version catalog carries no usable `id` — "+
				"the response is not the shape this was measured against, and a list shortened by a "+
				"parse failure must not be read as the account's full catalog", i, tier)
		}
		out = append(out, strings.TrimSpace(id))
	}
	return out, nil
}

// VersionVerdict is what an account's catalog can say about one pin.
//
// SHARED BY BOTH CALLERS, and that is the point: `llz doctor` prints this verdict
// and `llz ci assert-k8s-version` fails a build on it, so the two must not be able
// to disagree about the same spec.
//
// THREE OUTCOMES, NOT TWO, and that is the whole reason this replaced a bool.
//
// The bool version needed a companion guard — "is this catalog a shape I may
// judge?" — which every caller had to remember to ask FIRST and keep in agreement
// with the matcher. Three defects in two review rounds came from exactly that
// seam: the matcher decided precision per-entry while the guard decided it
// list-wide, and then the matcher let a build-less pin fall back to major.minor
// against a catalog that could have answered exactly. Both were "the guard and
// the matcher disagree", and neither was reachable as a bug until someone paired
// them wrong.
//
// One function now answers the whole question, so there is nothing left to pair.
type VersionVerdict int

const (
	// VersionUnknown — the catalog cannot settle it. An empty list, an
	// unparseable pin, or a catalog too coarse to disprove a build id. Callers
	// must report this and PASS: it is not evidence against the pin.
	VersionUnknown VersionVerdict = iota
	// VersionOffered — the account can build it.
	VersionOffered
	// VersionNotOffered — the account definitely cannot. Only ever returned
	// against a catalog naming full builds, which is the only shape that can
	// disprove anything.
	VersionNotOffered
)

// CheckVersion asks what the account's catalog says about want.
//
// ONLY AN EXACT MATCH CONFIRMS — byte for byte, after trimming surrounding space
// and nothing else. The catalog lists the strings the account will accept, and
// terraform hands `k8s_version` over verbatim, so "this pin appears in the list"
// is the only evidence that it can be built and every other agreement is an
// inference the catalog did not make. That includes the leading `v`: it looks like
// spelling, but the string is sent as written.
//
// The second return is the catalog entry a pin NEARLY matched — same version,
// different spelling of the `v`. It sharpens the MESSAGE, never the verdict: a
// near miss is rejected like any other absent pin, and the entry is there so a
// caller can name the one character to change instead of printing the catalog.
//
// A BUILD-NAMING CATALOG DISPROVES; A COARSER ONE CANNOT. When the entries are
// full LKE-E builds (`v1.34.6+lke2`), the list is authoritative at the precision
// the pin is written in, so an absent pin is a definite negative — including a pin
// that merely forgot its suffix, which is a rejection rather than a near miss.
// When the entries are bare `1.33`, the list cannot speak at build precision at
// all: it neither confirms `v1.33.6+lke7` nor rejects it, so the answer is
// UNKNOWN and the callers warn and pass.
//
// THREE WRONG VERSIONS OF THIS RULE SHIPPED BEFORE THIS ONE, all of them letting
// a pin through that the apply then rejected, and all from the same instinct —
// treating "close enough" as an answer:
//
//   - deciding build-vs-major.minor PER ENTRY, so one coarse row in an otherwise
//     build-id catalog re-opened loose matching for the whole comparison;
//   - requiring BOTH sides to name a build, so `v1.34.6` against an account
//     offering only `v1.34.6+lke2` passed on major.minor;
//   - treating major.minor agreement with a COARSE catalog as confirmation, so
//     `["1.33"]` reported the retired `v1.33.6+lke7` as offered — an unqualified
//     pass, and the apply still died at ~15 minutes with the 400.
//
// The last one is the subtlest, because it looked like the safe direction: it
// applied the uncertainty rule to disagreement only. A catalog too coarse to
// reject a build is equally too coarse to endorse one, and the difference between
// "UNCHECKED" and "is offered" is the difference between an operator who knows to
// look and one who does not.
func CheckVersion(want string, offered []string) (VersionVerdict, string) {
	w := strings.TrimSpace(want)
	if w == "" || len(offered) == 0 {
		return VersionUnknown, ""
	}
	nearest := ""
	for _, o := range offered {
		e := strings.TrimSpace(o)
		if e == w {
			return VersionOffered, ""
		}
		// A NEAR MISS IS STILL A MISS. A pin differing only in the leading `v` is not
		// in the catalog, and the catalog IS the list of strings the account accepts —
		// so it is rejected like any other absent pin, for the same reason a pin
		// missing its `+lke` suffix is.
		//
		// THE ASYMMETRY HAS NOW BEEN WRONG IN BOTH DIRECTIONS, which is why it is
		// spelled out rather than left to read as obvious. First the `v` was
		// normalised away on both sides, so `1.34.6+lke2` matched `v1.34.6+lke2` and
		// passed — and terraform sent the v-less string verbatim into the same 400.
		// Then it abstained on that case while still hard-rejecting a missing `+lke`,
		// which is two equally unmeasured spellings judged by different rules. The
		// catalog answers for both.
		//
		// What the near miss buys is the MESSAGE, not the verdict: the entry it almost
		// matched comes back so a caller can name the one character to change instead
		// of printing the whole catalog.
		if nearest == "" && normalizeVersion(e) == normalizeVersion(w) {
			nearest = e
		}
	}
	// THE LICENCE TO REJECT COMES FROM THE CATALOG, INCLUDING FOR A NEAR MISS. This
	// used to return NotOffered on a near miss BEFORE asking whether the catalog was
	// entitled to reject anything, so a coarse or mixed list hard-failed a build:
	// CheckVersion("v1.33", ["v1.34.6+lke2", "1.33"]) came back NotOffered against
	// the very shape this file abstains on — and the message then told the operator
	// to pin `1.33`, which the LKE-E create API will not accept.
	//
	// A near miss sharpens the message; it never widens who may reject.
	if everyEntryNamesABuild(offered) {
		return VersionNotOffered, nearest
	}
	// Deliberately dropped: pointing at a coarse entry as "the spelling that works"
	// would be advice to pin a string the API rejects.
	return VersionUnknown, ""
}

// everyEntryNamesABuild reports whether the catalog is the shape this was measured
// against, all the way through — full LKE-E build ids (`v1.34.6+lke2`) and nothing
// coarser.
//
// EVERY, NOT ANY, AND THE DIFFERENCE IS A HARD BUILD FAILURE. Rejecting is the
// only destructive thing this file does, so the licence to reject has to come from
// a catalog that can express what it is rejecting — all of it. Under ANY
// semantics a MIXED list (`["v1.34.6+lke2", "1.33"]`) licensed `NotOffered` for a
// pin the `1.33` row may well cover, which contradicts this file's own rule that
// an entry carrying no build suffix is uncertainty and never a verdict.
//
// It is not the mirror of the confirming side, and does not need to be:
// confirmation is exact-match only, so a coarse row cannot smuggle a pass in
// either. A mixed catalog is simply a shape nobody has measured, and the answer
// for those is UNKNOWN in both directions.
//
// UNEXPORTED ON PURPOSE. It used to be exported so callers could pre-screen the
// catalog before matching, and that is precisely the seam CheckVersion exists to
// close: a predicate a caller can forget, or pair with a matcher that answers the
// same question differently, is a defect waiting for its second author.
func everyEntryNamesABuild(offered []string) bool {
	seen := false
	for _, o := range offered {
		n := normalizeVersion(o)
		if n == "" {
			continue
		}
		if !hasBuild(n) {
			return false
		}
		seen = true
	}
	return seen
}

// ClusterReadAttempts is how many times the existing-cluster exemption may ask
// before giving up, and ClusterReadRetryPause is the gap between tries.
//
// THE EXEMPTION IS A PROOF, AND A SINGLE-SHOT READ MAKES IT TOO EASY TO FAIL TO
// PROVE. Callers correctly refuse to let an unprovable exemption acquit a pin the
// catalog already disproved — but with one attempt, a single 503 or a rate-limit
// on /lke/clusters convicts a long-lived deployment whose cluster is sitting
// right there at the pinned version, and blocks the apply it was running fine
// without. Bounded by ATTEMPTS rather than by a deadline: a retry loop that
// counts wall-clock spins for free when the clock is stubbed.
const (
	ClusterReadAttempts   = 2
	ClusterReadRetryPause = 2 * time.Second
)

// ClusterRunsVersion reports whether exactly one cluster on the account matches
// label (and region, when given) AND already runs version.
//
// IT IS THE "TERRAFORM WILL NOT SEND THIS STRING" TEST, and without it the
// preflight blocks work it has no business blocking. `k8s_version` is handed to
// the API only on CREATE, or on an UPDATE that changes it — for an existing
// cluster already at the pin, terraform plans no diff and the apply succeeds
// whatever the catalog says today. LKE-E versions rotate fast (this repo measured
// one leaving an account's catalog inside an hour), so without this exemption
// every routine apply to a long-lived deployment — a node-pool resize, an ACL
// change — would be blocked until someone bumped `cluster.k8sVersion`, forcing a
// control-plane upgrade nobody asked for.
//
// IT INFERS THAT NO-DIFF FROM THE LIVE ACCOUNT, NOT FROM A PLAN, and that gap is
// real rather than theoretical. Two shapes defeat it, and both end in the same
// `[400] k8s_version is not valid` fifteen minutes in:
//
//   - an ORPHANED cluster left at this label+region by a failed cycle — this repo
//     has a whole triage runbook for those — satisfies this while the tfstate that
//     would adopt it is gone, so terraform plans a CREATE rather than a no-op;
//   - a planned REPLACEMENT re-sends `k8s_version` on the create half. Flipping
//     `cluster.bootstrap.managedAppPlatform` (`apl_enabled` is ForceNew) or moving
//     the deployment's VPC/subnet binding does that, so a cluster sitting at a
//     rotated-out pin is exempted here and then destroyed and recreated at it.
//
// Closing either needs a plan, and there is none: the preflight deliberately runs
// before any root is initialised, which is the whole reason it costs seconds. So
// the exemption is the cheaper inference on purpose — it trades a COMMON false
// failure (every routine apply after a rotation, on every long-lived deployment)
// for a RARE false pass whose cost is the same fifteen minutes this gate normally
// saves. Sweeping orphans (`llz reap`) closes the first; the second is bounded to
// changes that already imply a rebuild, where a stale pin is the operator's next
// question anyway.
//
// ONE ASSUMPTION IS UNMEASURED AND IT FAILS SAFE. The CATALOG's spelling has been
// seen (full `v…+lke…` ids); the CLUSTER OBJECT's `k8s_version` has not — the only
// in-repo corroboration is credrotate/lkeadmin.go, which merely checks it contains
// "+lke". If LKE-E reported it without the leading `v`, this would silently never
// fire and every routine apply to a long-lived deployment would block after a
// rotation. That is the LOUD direction (a red gate naming the offered versions),
// not a silent pass, which is why it is written exactly rather than loosened on
// a guess: the terraform provider round-trips this value into state, so it almost
// certainly echoes verbatim, and if it does not the failure says so immediately.
//
// EXACTLY ONE MATCH, deliberately. Zero means there is nothing to no-op and the
// pin must be buildable; more than one is an ambiguous account this must not
// guess about. Both fall through to the catalog verdict, which is the safe
// direction.
func ClusterRunsVersion(clusters []map[string]any, label, region, version string) bool {
	// EXACT, for the same reason CheckVersion is: this decides whether terraform
	// PLANS A DIFF, and terraform compares the tfvars string against what the API
	// reports. A pin spelled `1.34.6+lke2` against a cluster reporting
	// `v1.34.6+lke2` is a diff — so normalising the `v` here would exempt precisely
	// the deployment terraform is about to send the pin for.
	w := strings.TrimSpace(version)
	if w == "" || label == "" {
		return false
	}
	var found int
	var running string
	for _, m := range clusters {
		if mString(m, "label") != label {
			continue
		}
		if region != "" && mString(m, "region") != region {
			continue
		}
		found++
		running = strings.TrimSpace(mString(m, "k8s_version"))
	}
	return found == 1 && running == w
}

// normalizeVersion trims surrounding space and the optional leading `v`.
//
// IT IS NOT A COMPARISON HELPER ANY MORE, and must not become one again: both
// CheckVersion and ClusterRunsVersion compare the strings terraform actually
// sends, `v` included. Its one remaining job is spotting a NEAR MISS — a pin that
// differs from a catalog entry only in that character — so the caller can name it.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// hasBuild reports whether a normalized version carries an `+lke…` build suffix.
func hasBuild(v string) bool { return strings.Contains(v, "+") }

// NamesABuild reports whether v is spelled as a full LKE-E build id rather than a
// bare major.minor.
//
// EXPORTED FOR THE MESSAGE, NEVER FOR THE VERDICT. CheckVersion owns whether a pin
// is offered, and nothing here may second-guess it — the whole point of that
// function is that doctor and the preflight cannot reach different conclusions
// about one spec. What a caller legitimately needs to know separately is whether
// the string it is about to CALL CONFIRMED is one the create API could accept:
// `llz env add` told an operator "confirmed against the account's catalog" about a
// pin of `1.33` that byte-matched a coarse `1.33` row, and then wrote it into the
// spec for terraform to send verbatim.
func NamesABuild(v string) bool { return hasBuild(normalizeVersion(v)) }

// NewestVersion returns the newest FULL BUILD ID in an account's catalog, or ""
// when the catalog holds none.
//
// IT PICKS A STRING THE ACCOUNT LITERALLY LISTED, which is the whole safety
// argument and the reason it may be laxer than CheckVersion about a mixed
// catalog. CheckVersion's job is to REJECT, so it needs a list entitled to
// disprove — `everyEntryNamesABuild`, all of it. This one's job is to CHOOSE, and
// a build id copied verbatim out of the catalog is buildable regardless of what
// the rows beside it look like. So a coarse row is skipped rather than
// disqualifying the list: `["v1.34.6+lke2", "1.33"]` yields `v1.34.6+lke2`, which
// is a version the account offers, while `["1.34", "1.33"]` yields "" because
// nothing there is a string terraform could send. The asymmetry is deliberate;
// it is not this file forgetting its own rule.
//
// IT DOES NOT TRUST THE API'S ORDER. ListLKEVersions documents the catalog as
// newest-first "as the API returns them", which is the Linode list convention and
// not a guarantee anyone here has measured across releases. Seeding a scaffold off
// an assumed sort order would fail silently and in the expensive direction — an
// instance pinned to the OLDEST version its account offers, discovered a minor
// upgrade later. Parse and compare instead; it costs nothing.
//
// TIES AND UNPARSEABLE ROWS. Entries that do not parse as v?MAJOR.MINOR.PATCH+lkeN
// are skipped, not guessed at. When two entries compare equal the FIRST wins, so
// the API's order still breaks ties — the only thing it is trusted for.
func NewestVersion(offered []string) string {
	best, bestKey := "", [4]int{-1, -1, -1, -1}
	for _, o := range offered {
		e := strings.TrimSpace(o)
		key, ok := versionSortKey(e)
		if !ok {
			continue
		}
		if best == "" || key.greaterThan(bestKey) {
			best, bestKey = e, key
		}
	}
	return best
}

// versionKey is (major, minor, patch, lke build) — the four numbers an LKE-E build
// id carries, in the order they rank.
type versionKey [4]int

func (k versionKey) greaterThan(other [4]int) bool {
	for i := range k {
		if k[i] != other[i] {
			return k[i] > other[i]
		}
	}
	return false
}

// versionSortKey parses `v1.34.6+lke2` into its four numbers.
//
// ok is false for ANYTHING ELSE, including a version carrying no `+lke` build —
// deliberately, because the caller is choosing a string to write into a spec and
// terraform hands it to the create API verbatim. A coarse entry is a row this
// cannot turn into a buildable pin, so it is not a candidate.
func versionSortKey(v string) (versionKey, bool) {
	var k versionKey
	n := strings.TrimPrefix(strings.TrimSpace(v), "v")
	base, build, found := strings.Cut(n, "+lke")
	if !found {
		return k, false
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return k, false
	}
	for i, p := range append(parts, build) {
		num, err := strconv.Atoi(p)
		if err != nil || num < 0 {
			return k, false
		}
		k[i] = num
	}
	return k, true
}
