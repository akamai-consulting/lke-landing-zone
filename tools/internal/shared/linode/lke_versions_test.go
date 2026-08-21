package linode

// lke_versions_test.go — the catalog verdict, which moved here when `llz ci
// assert-k8s-version` started FAILING builds on the same answer `llz doctor` had
// only ever printed. Two callers, one rule, one place it is tested.

import "testing"

// theE2EAccountCatalog is what /v4beta/lke/tiers/enterprise/versions returned on
// 2026-08-11, in the same hour another account still offered v1.33.6+lke7.
var theE2EAccountCatalog = []string{"v1.34.6+lke2", "v1.32.9+lke4"}

func (v VersionVerdict) String() string {
	switch v {
	case VersionOffered:
		return "Offered"
	case VersionNotOffered:
		return "NotOffered"
	}
	return "Unknown"
}

// THE RULE THAT CHANGED, AND THE MEASUREMENT THAT CHANGED IT. This used to accept
// any major.minor agreement, because the enterprise catalog's spelling was unknown
// and crying wolf was worse than silence for an advisory print. The catalog has
// since been measured (see lke_versions.go): it returns full `+lke` build ids, and
// terraform hands the pin to `k8s_version` verbatim — so against a build-naming
// catalog the build IS the identity, and anything else is a failed apply rather
// than a near miss.
func TestCheckVersionAgainstABuildNamingCatalog(t *testing.T) {
	for want, verdict := range map[string]VersionVerdict{
		// Offered: byte-for-byte, modulo surrounding space.
		"v1.34.6+lke2":   VersionOffered,
		" v1.32.9+lke4 ": VersionOffered,

		// THE LEADING `v` IS PART OF THE STRING TERRAFORM SENDS, so a pin without it
		// is not in the catalog and is rejected like any other absent pin — the same
		// rule that rejects a pin missing its `+lke` suffix. This has been wrong in
		// both directions: first normalised away on both sides (a free pass into the
		// 400), then abstained on while its sibling stayed fatal.
		"1.34.6+lke2": VersionNotOffered,

		// The pin that cost a release-e2e round, plus same-minor different-build
		// near misses the API rejects just as hard.
		"v1.33.6+lke7": VersionNotOffered,
		"v1.34.6+lke1": VersionNotOffered,
		"v1.32.9+lke3": VersionNotOffered,

		// THE MISTYPED-PIN HALF. A pin that merely forgot its `+lke` suffix used to
		// fall back to major.minor and pass. terraform sends it verbatim, so it dies
		// with `[400] [k8s_version] k8s_version is not valid` — and nothing else in
		// the repo catches the shape: clusterspec's validate checks non-emptiness
		// only, and neither TF root constrains it.
		"v1.34.6":      VersionNotOffered,
		"1.34.6":       VersionNotOffered,
		"v1.34":        VersionNotOffered,
		"v1.34.6+lke":  VersionNotOffered,
		"v1.34.6-lke2": VersionNotOffered,
		"garbage":      VersionNotOffered,

		// Nothing to judge at all.
		"":    VersionUnknown,
		"   ": VersionUnknown,
	} {
		if got, _ := CheckVersion(want, theE2EAccountCatalog); got != verdict {
			t.Errorf("CheckVersion(%q, %v) = %s, want %s", want, theE2EAccountCatalog, got, verdict)
		}
	}
}

// A COARSER CATALOG CANNOT SPEAK AT BUILD PRECISION AT ALL — in either direction.
// It has no standing to reject a `+lke` build (blocking on a spelling it cannot
// express is the failure this whole check is written around) and equally none to
// endorse one, which is the half that took three tries to get right.
func TestCheckVersionAgainstACoarseCatalog(t *testing.T) {
	coarse := []string{"1.32", "1.33"}
	for want, verdict := range map[string]VersionVerdict{
		"1.33": VersionOffered, // the catalog literally lists this string

		// AGREEMENT AT A COARSER PRECISION IS NOT CONFIRMATION. This used to return
		// Offered — an unqualified pass for a retired build the account may well not
		// have, and the apply still died at ~15 min with the 400. A list too coarse to
		// REJECT a build is equally too coarse to ENDORSE one.
		"v1.33.6+lke7": VersionUnknown,
		"1.33.6":       VersionUnknown,

		// Disagreement is uncertainty too: this list cannot reject a build.
		"v1.30.1+lke1": VersionUnknown,
		"1.29":         VersionUnknown,
		"garbage":      VersionUnknown,
	} {
		if got, _ := CheckVersion(want, coarse); got != verdict {
			t.Errorf("CheckVersion(%q, %v) = %s, want %s", want, coarse, got, verdict)
		}
	}

	// A MIXED CATALOG IS UNKNOWN IN BOTH DIRECTIONS. It cannot CONFIRM (only an
	// exact match does that, so the coarse row cannot smuggle a pass in) and it may
	// not REJECT either — the `1.33` row may well cover the pin, and rejecting is
	// the only destructive thing this makes possible. An earlier rule licensed
	// NotOffered from any single build id in the list, which turned an unmeasured
	// shape into a hard build failure.
	mixed := []string{"v1.34.6+lke2", "1.33"}
	if got, _ := CheckVersion("v1.33.6+lke7", mixed); got != VersionUnknown {
		t.Errorf("CheckVersion(%q, %v) = %s, want Unknown — the coarse row may cover the pin, "+
			"so this list cannot reject it; and it cannot confirm it either", "v1.33.6+lke7", mixed, got)
	}
	if got, _ := CheckVersion("v1.34.6+lke2", mixed); got != VersionOffered {
		t.Errorf("CheckVersion(%q, %v) = %s, want Offered — an exact match is an exact match",
			"v1.34.6+lke2", mixed, got)
	}
}

// An empty catalog is never an affirmative and never a rejection. Callers report
// it as uncertainty; the predicate must not claim either.
func TestCheckVersionOnAnEmptyCatalog(t *testing.T) {
	for _, offered := range [][]string{nil, {}, {""}, {"   "}} {
		if got, _ := CheckVersion("v1.33.6+lke7", offered); got != VersionUnknown {
			t.Errorf("CheckVersion against %v = %s, want Unknown", offered, got)
		}
	}
}

// THE HOLE THAT OPENED WHEN THE LIST STOPPED BEING ADVISORY. ListLKEVersions used
// to drop a row it could not parse and return the rest — harmless for a print,
// and a build failure for `llz ci assert-k8s-version`, which reads absence as a
// verdict. The pin the account actually offers would have been the dropped row.
func TestListLKEVersionsRefusesAPartialCatalog(t *testing.T) {
	for _, raw := range []map[string]any{
		{"id": nil},
		{"id": 133},
		{"id": "   "},
		{"version": "v1.34.6+lke2"}, // renamed field
	} {
		got, err := parseLKEVersions("enterprise", []map[string]any{{"id": "v1.34.6+lke2"}, raw})
		if err == nil {
			t.Errorf("a row this cannot read (%v) returned a SHORTENED list (%v) instead of an "+
				"error — the caller would report a version the account offers as unbuildable", raw, got)
		}
	}
	got, err := parseLKEVersions("enterprise", []map[string]any{{"id": "v1.34.6+lke2"}, {"id": " v1.32.9+lke4 "}})
	if err != nil {
		t.Fatalf("a well-formed catalog must parse: %v", err)
	}
	if len(got) != 2 || got[0] != "v1.34.6+lke2" || got[1] != "v1.32.9+lke4" {
		t.Errorf("parsed %v, want the two ids trimmed and in order", got)
	}
	// An empty response is not a parse failure — it is the account saying it offers
	// nothing, which callers report as uncertainty rather than as a bad pin.
	if _, err := parseLKEVersions("enterprise", nil); err != nil {
		t.Errorf("an empty catalog is an answer, not a parse error: %v", err)
	}
}

// LOOSE MATCHING IS GONE, and this is what used to need guarding against. The
// old rule compared major.minor when it could not compare builds, so two
// unparseable strings could agree on a garbage prefix and declare a nonsense pin
// "offered" — which is why majorMinor had to fail closed on a leading separator.
// Only an exact match confirms now, so the whole class is structurally absent.
func TestNothingButAnExactMatchConfirms(t *testing.T) {
	for _, tc := range []struct {
		want    string
		offered []string
	}{
		{"-1.33", []string{"-1.33.6"}},        // garbage agreeing with garbage
		{"v1.33.6+lke7", []string{"1.33"}},    // coarser catalog
		{"1.33.6", []string{"1.33"}},          // coarser catalog, finer pin
		{"v1.33", []string{"v1.33.6+lke7"}},   // coarser pin
		{"v1.33.6", []string{"v1.33.6+lke7"}}, // pin missing its build
	} {
		if got, _ := CheckVersion(tc.want, tc.offered); got == VersionOffered {
			t.Errorf("CheckVersion(%q, %v) = Offered — nothing in that catalog is the string "+
				"terraform would send", tc.want, tc.offered)
		}
	}
	// Surrounding space is noise; the leading `v` is not.
	for _, want := range []string{"v1.33.6+lke7", "  v1.33.6+lke7  "} {
		if got, _ := CheckVersion(want, []string{"v1.33.6+lke7"}); got != VersionOffered {
			t.Errorf("CheckVersion(%q, [v1.33.6+lke7]) = %s, want Offered", want, got)
		}
	}
}

// A PIN ONE CHARACTER OFF IS REJECTED, AND THE CHARACTER IS NAMED. The near miss
// changes the MESSAGE, not the verdict — the catalog named this very version in a
// different spelling, so it can speak to the pin, and terraform sends the string
// as written.
func TestCheckVersionNamesASpellingNearMiss(t *testing.T) {
	got, nearest := CheckVersion("1.34.6+lke2", theE2EAccountCatalog)
	if got != VersionNotOffered {
		t.Errorf("CheckVersion = %s, want NotOffered — the catalog can speak to this pin", got)
	}
	if nearest != "v1.34.6+lke2" {
		t.Errorf("nearest = %q, want the entry it nearly matched", nearest)
	}
	// A real match reports no near miss, and a real mismatch has none to report.
	if _, n := CheckVersion("v1.34.6+lke2", theE2EAccountCatalog); n != "" {
		t.Errorf("an exact match has no near miss to name, got %q", n)
	}
	if v, n := CheckVersion("v1.33.6+lke7", theE2EAccountCatalog); v != VersionNotOffered || n != "" {
		t.Errorf("a pin nothing resembles is a plain rejection, got %s / %q", v, n)
	}
}

// The "will terraform even send this string" test. Exactly one match, or fall
// through to the catalog verdict — zero has nothing to no-op, and an ambiguous
// account is not something to guess about.
func TestClusterRunsVersion(t *testing.T) {
	one := []map[string]any{{"label": "llz-prod", "region": "us-ord", "k8s_version": "v1.33.6+lke7"}}
	if !ClusterRunsVersion(one, "llz-prod", "us-ord", "v1.33.6+lke7") {
		t.Error("the cluster is at the pin; terraform plans no change to k8s_version")
	}
	if !ClusterRunsVersion(one, "llz-prod", "", "v1.33.6+lke7") {
		t.Error("an empty region must not narrow the match")
	}
	// EXACT here too: this decides whether terraform plans a diff, and terraform
	// compares the tfvars string against what the API reports. Exempting a v-less
	// pin would exempt precisely the deployment about to send it.
	if ClusterRunsVersion(one, "llz-prod", "us-ord", "1.33.6+lke7") {
		t.Error("a pin spelled without the leading v IS a diff against a cluster reporting it " +
			"with one — terraform will send the pin, so this may not exempt it")
	}
	for name, args := range map[string][3]string{
		"other label":   {"llz-lab", "us-ord", "v1.33.6+lke7"},
		"other region":  {"llz-prod", "us-sea", "v1.33.6+lke7"},
		"other version": {"llz-prod", "us-ord", "v1.32.9+lke4"},
		"no label":      {"", "us-ord", "v1.33.6+lke7"},
		"no version":    {"llz-prod", "us-ord", ""},
	} {
		if ClusterRunsVersion(one, args[0], args[1], args[2]) {
			t.Errorf("%s: matched a cluster it should not have — terraform WILL send the pin", name)
		}
	}
	two := append(append([]map[string]any{}, one...), one...)
	if ClusterRunsVersion(two, "llz-prod", "us-ord", "v1.33.6+lke7") {
		t.Error("two clusters share the label+region — an ambiguous account must fall through " +
			"to the catalog verdict rather than be guessed about")
	}
}

// TestClusterVersionForIsTheOneMatchingRule.
//
// ClusterRunsVersion answers a bool, which is all the preflight and `llz doctor`
// need. `llz env add` needs the VERSION — it is choosing a pin, not judging one —
// so #453 extracted the matcher rather than writing a second loop over label+region.
// A second loop would drift invisibly: both would still be "right" about the
// account and disagree about which cluster belongs to a deployment.
func TestClusterVersionForIsTheOneMatchingRule(t *testing.T) {
	one := []map[string]any{{"label": "llz-prod", "region": "us-ord", "k8s_version": "v1.33.6+lke7"}}
	if got := ClusterVersionFor(one, "llz-prod", "us-ord"); got != "v1.33.6+lke7" {
		t.Errorf("ClusterVersionFor = %q, want v1.33.6+lke7", got)
	}
	if got := ClusterVersionFor(one, "llz-prod", ""); got != "v1.33.6+lke7" {
		t.Errorf("an empty region must not narrow the match; got %q", got)
	}
	for name, args := range map[string][2]string{
		"other label":  {"llz-lab", "us-ord"},
		"other region": {"llz-prod", "us-sea"},
		// AN EMPTY LABEL MATCHES NOTHING. Callers derive it from a spec or from the
		// instance identity, and both can come back empty on a malformed tree —
		// without this, a one-cluster account would hand back a confident answer
		// about a deployment nobody named.
		"no label": {"", "us-ord"},
	} {
		if got := ClusterVersionFor(one, args[0], args[1]); got != "" {
			t.Errorf("%s: ClusterVersionFor = %q, want \"\"", name, got)
		}
	}
	two := append(append([]map[string]any{}, one...), one...)
	if got := ClusterVersionFor(two, "llz-prod", "us-ord"); got != "" {
		t.Errorf("two clusters share the label+region — an ambiguous account is not an answer; got %q", got)
	}

	// AND ClusterRunsVersion IS STILL EXPRESSED IN IT rather than beside it: the two
	// must not be able to disagree about which cluster a deployment owns.
	for _, c := range [][]map[string]any{one, two, nil} {
		for _, label := range []string{"llz-prod", "llz-lab", ""} {
			want := ClusterVersionFor(c, label, "us-ord") == "v1.33.6+lke7"
			if got := ClusterRunsVersion(c, label, "us-ord", "v1.33.6+lke7"); got != want {
				t.Errorf("ClusterRunsVersion(%q) = %v but ClusterVersionFor says %v — two rules for one match",
					label, got, want)
			}
		}
	}
}

// TestOneMatcherServesEveryCallerThatResolvesADeploymentsCluster.
//
// The label+region predicate was written TWICE before anyone noticed — here and in
// acl.go's MatchClusterIDs — and the second copy arrived in a change whose own
// comment claimed there was only one. Neither would ever have looked wrong; they
// would simply have answered differently about one deployment after an edit to one
// of them, which is precisely the drift #443 exists to prevent.
//
// So this asserts the two agree by CONSTRUCTION, over shapes that separate them:
// an exact match, a region mismatch, an ambiguous pair, and the empty label — which
// is the one place they legitimately differ, and the difference is documented on
// ClusterVersionFor rather than accidental.
func TestOneMatcherServesEveryCallerThatResolvesADeploymentsCluster(t *testing.T) {
	clusters := []map[string]any{
		{"id": 1, "label": "llz-prod", "region": "us-ord", "k8s_version": "v1.33.6+lke7"},
		{"id": 2, "label": "llz-lab", "region": "us-sea", "k8s_version": "v1.32.9+lke4"},
		{"id": 3, "label": "llz-dup", "region": "us-ord", "k8s_version": "v1.33.6+lke7"},
		{"id": 4, "label": "llz-dup", "region": "us-ord", "k8s_version": "v1.32.9+lke4"},
	}
	for _, tc := range []struct {
		label, region string
		wantMatches   int
	}{
		{"llz-prod", "us-ord", 1},
		{"llz-prod", "", 1},
		{"llz-prod", "us-sea", 0},
		{"llz-lab", "us-sea", 1},
		{"llz-dup", "us-ord", 2}, // ambiguous — a real shape, not a contrivance
		{"nope", "us-ord", 0},
	} {
		m := MatchingClusters(clusters, tc.label, tc.region)
		if len(m) != tc.wantMatches {
			t.Errorf("MatchingClusters(%q, %q) matched %d, want %d", tc.label, tc.region, len(m), tc.wantMatches)
		}
		// MatchClusterIDs is the same predicate projected onto ids.
		if ids := MatchClusterIDs(clusters, tc.label, tc.region); len(ids) != len(m) {
			t.Errorf("MatchClusterIDs(%q, %q) matched %d but MatchingClusters matched %d — the two "+
				"resolve-by-label rules have drifted apart", tc.label, tc.region, len(ids), len(m))
		}
		// And ClusterVersionFor answers iff the match is UNIQUE.
		got := ClusterVersionFor(clusters, tc.label, tc.region)
		if (got != "") != (tc.wantMatches == 1) {
			t.Errorf("ClusterVersionFor(%q, %q) = %q with %d match(es) — it must answer only on exactly one",
				tc.label, tc.region, got, tc.wantMatches)
		}
	}
	// THE ONE DELIBERATE DIVERGENCE. MatchingClusters compares "" as a label, which
	// is right for a caller looking for an unlabelled cluster; ClusterVersionFor
	// refuses, because "" there means the spec could not be read and every cluster
	// with no label would otherwise answer for a deployment nobody named.
	unlabelled := []map[string]any{{"id": 9, "region": "us-ord", "k8s_version": "v1.33.6+lke7"}}
	if len(MatchingClusters(unlabelled, "", "us-ord")) != 1 {
		t.Error("MatchingClusters must still compare the empty label literally")
	}
	if ClusterVersionFor(unlabelled, "", "us-ord") != "" {
		t.Error("ClusterVersionFor must refuse an empty label rather than answer off an unlabelled cluster")
	}
}

// A NEAR MISS SHARPENS THE MESSAGE; IT NEVER WIDENS WHO MAY REJECT. The near-miss
// branch used to return NotOffered before asking whether the catalog was entitled
// to reject anything, so a coarse or mixed list hard-failed a build — and the
// message then pointed at a coarse entry as "the spelling that works", which is
// advice to pin a string the LKE-E create API will not accept.
func TestANearMissCannotLicenseARejectionACoarseCatalogCouldNotMake(t *testing.T) {
	for _, offered := range [][]string{
		{"v1.34.6+lke2", "1.33"}, // mixed
		{"1.33", "1.32"},         // coarse
	} {
		got, nearest := CheckVersion("v1.33", offered)
		if got != VersionUnknown {
			t.Errorf("CheckVersion(\"v1.33\", %v) = %s, want Unknown — this list cannot reject", offered, got)
		}
		if nearest != "" {
			t.Errorf("naming %q as the spelling that works is advice to pin a string the API "+
				"rejects", nearest)
		}
	}
	// The build-naming catalog still both rejects AND names the near miss.
	got, nearest := CheckVersion("1.34.6+lke2", theE2EAccountCatalog)
	if got != VersionNotOffered || nearest != "v1.34.6+lke2" {
		t.Errorf("CheckVersion = %s / %q, want NotOffered / v1.34.6+lke2", got, nearest)
	}
}
