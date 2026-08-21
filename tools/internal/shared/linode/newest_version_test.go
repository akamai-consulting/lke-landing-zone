package linode

import "testing"

// theOtherAccountCatalog is the second account measured on 2026-08-11 (see
// lke_versions_test.go): in the SAME HOUR it still offered v1.33.6+lke7, which the
// e2e account did not. It is the fixture that keeps "the account decides" honest.
var theOtherAccountCatalog = []string{"v1.33.6+lke7"}

func TestNewestVersionPicksTheHighestBuildWhateverOrderTheAPIUsed(t *testing.T) {
	// ListLKEVersions documents the catalog as newest-first "as the API returns
	// them" — the Linode list convention, not a guarantee measured across releases.
	// Both orders must give the same answer, because seeding a scaffold off an
	// assumed sort order fails SILENTLY and in the expensive direction: an instance
	// pinned to the OLDEST version its account offers, found out a minor later.
	for _, catalog := range [][]string{
		{"v1.34.6+lke2", "v1.32.9+lke4"},
		{"v1.32.9+lke4", "v1.34.6+lke2"},
	} {
		if got := NewestVersion(catalog); got != "v1.34.6+lke2" {
			t.Errorf("NewestVersion(%v) = %q, want v1.34.6+lke2", catalog, got)
		}
	}
	if got := NewestVersion(theOtherAccountCatalog); got != "v1.33.6+lke7" {
		t.Errorf("NewestVersion(%v) = %q, want v1.33.6+lke7", theOtherAccountCatalog, got)
	}
}

func TestNewestVersionRanksNumericallyAndNotLexically(t *testing.T) {
	// EVERY ONE OF THESE IS A PAIR STRING COMPARISON GETS WRONG, which is the whole
	// reason the ids are parsed instead of sorted. `"v1.34.6+lke2" > "v1.34.6+lke10"`
	// is true as strings and false as versions, and the wrong answer here is a spec
	// pinned to a superseded build for the life of the instance.
	for _, c := range []struct {
		catalog []string
		want    string
	}{
		{[]string{"v1.34.6+lke2", "v1.34.6+lke10"}, "v1.34.6+lke10"}, // build 10 > 2
		{[]string{"v1.34.9+lke1", "v1.34.10+lke1"}, "v1.34.10+lke1"}, // patch 10 > 9
		{[]string{"v1.9.0+lke1", "v1.10.0+lke1"}, "v1.10.0+lke1"},    // minor 10 > 9
		{[]string{"v2.0.0+lke1", "v1.99.99+lke9"}, "v2.0.0+lke1"},    // major wins
	} {
		if got := NewestVersion(c.catalog); got != c.want {
			t.Errorf("NewestVersion(%v) = %q, want %q", c.catalog, got, c.want)
		}
	}
}

func TestNewestVersionReturnsTheCatalogsOwnSpelling(t *testing.T) {
	// The point of picking FROM the catalog is that the string is one the account
	// listed; terraform sends cluster.k8sVersion verbatim, so anything this
	// normalises or reconstructs is a pin nobody has evidence for.
	if got := NewestVersion([]string{"  v1.34.6+lke2  "}); got != "v1.34.6+lke2" {
		t.Errorf("NewestVersion trimmed-entry = %q, want the entry itself", got)
	}
}

func TestNewestVersionRefusesACatalogItCannotTurnIntoABuildablePin(t *testing.T) {
	// "" MEANS "keep your own default" and the caller depends on it. A coarse
	// catalog cannot yield a string the LKE-E create API accepts, so guessing one
	// would trade a stale literal for a pin that is certainly rejected — strictly
	// worse. Contrast NewestVersion's laxness about a MIXED list below: this is the
	// same rule, not a different one. A build id copied out of the catalog is
	// buildable whatever sits beside it; a coarse row is not a candidate at all.
	for _, catalog := range [][]string{
		nil,
		{},
		{"1.33", "1.34"},         // no builds at all
		{"v1.34.6", "v1.32.9"},   // versions without the +lke suffix
		{"", "   "},              // blank rows
		{"latest", "v1.34+lke2"}, // unparseable, and a base with too few parts
		{"v1.34.6+lkeX"},         // non-numeric build
		{"v1.34.-6+lke2"},        // negative component
	} {
		if got := NewestVersion(catalog); got != "" {
			t.Errorf("NewestVersion(%v) = %q, want \"\" — nothing there is a string terraform could send", catalog, got)
		}
	}
}

func TestNewestVersionSkipsCoarseRowsRatherThanBeingDisqualifiedByThem(t *testing.T) {
	// DELIBERATELY LAXER THAN CheckVersion's everyEntryNamesABuild, and the
	// asymmetry is the design. CheckVersion REJECTS, so it needs a list entitled to
	// disprove — all of it. This CHOOSES, and v1.34.6+lke2 is a version the account
	// listed regardless of the row beside it.
	mixed := []string{"v1.34.6+lke2", "1.33"}
	if got := NewestVersion(mixed); got != "v1.34.6+lke2" {
		t.Errorf("NewestVersion(%v) = %q, want v1.34.6+lke2 — a coarse row is skipped, not disqualifying", mixed, got)
	}
	// …and the same list is still UNKNOWN to the matcher, because rejecting is the
	// destructive direction. Both callers must be able to hold both rules at once.
	if v, _ := CheckVersion("v1.33.6+lke7", mixed); v != VersionUnknown {
		t.Errorf("CheckVersion against a mixed catalog = %v, want VersionUnknown", v)
	}
}

func TestNewestVersionBreaksTiesOnTheAPIsOrder(t *testing.T) {
	// The only thing the API's order is trusted for. Two entries that compare equal
	// are the same version, so either is correct — first wins so the result is
	// deterministic rather than dependent on iteration.
	dup := []string{"v1.34.6+lke2", "1.34.6+lke2"}
	if got := NewestVersion(dup); got != "v1.34.6+lke2" {
		t.Errorf("NewestVersion(%v) = %q, want the first entry", dup, got)
	}
}

// TestNewestVersionInMinorOfIsNewestVersionNarrowedToOneMinor — it must agree with
// NewestVersion on RANKING (numeric, so +lke10 beats +lke2) and differ only in
// which rows it will consider. A second ranking implementation that drifts from
// the first is how "the newest of the 1.32 builds" quietly becomes the oldest.
func TestNewestVersionInMinorOfIsNewestVersionNarrowedToOneMinor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offered []string
		like    string
		want    string
	}{
		{"picks within the minor, not the catalog's newest",
			[]string{"v1.34.6+lke2", "v1.32.9+lke4"}, "v1.32.5+lke1", "v1.32.9+lke4"},
		{"ranks builds numerically inside the minor",
			[]string{"v1.32.9+lke2", "v1.32.9+lke10"}, "v1.32.1+lke1", "v1.32.9+lke10"},
		{"ranks patches inside the minor",
			[]string{"v1.32.10+lke1", "v1.32.9+lke9"}, "v1.32.1+lke1", "v1.32.10+lke1"},
		// THE REFERENCE IS READ, NOT CHOSEN, so it only has to name a minor — and the
		// two spellings below are precisely the ones an account REJECTS, which is when
		// a replacement is being looked for at all. Requiring a build id here would
		// send exactly those cases to the account's absolute newest.
		{"a build-less reference still names its minor",
			[]string{"v1.32.9+lke4", "v1.34.6+lke2"}, "v1.32.5", "v1.32.9+lke4"},
		{"a v-less reference still names its minor",
			[]string{"v1.32.9+lke4", "v1.34.6+lke2"}, "1.32.5+lke1", "v1.32.9+lke4"},
		{"a bare major.minor still names its minor",
			[]string{"v1.32.9+lke4", "v1.34.6+lke2"}, "1.32", "v1.32.9+lke4"},
		{"a major bump is a different minor",
			[]string{"v2.32.9+lke4"}, "v1.32.5+lke1", ""},
		{"the minor is absent", []string{"v1.34.6+lke2"}, "v1.32.5+lke1", ""},
		{"coarse rows are skipped here too", []string{"1.32", "1.32.9"}, "v1.32.5+lke1", ""},
		{"a reference naming no minor at all", []string{"v1.32.9+lke4"}, "stable", ""},
		{"a reference with a non-numeric minor", []string{"v1.32.9+lke4"}, "v1.x", ""},
		{"an empty catalog", nil, "v1.32.5+lke1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewestVersionInMinorOf(tc.offered, tc.like); got != tc.want {
				t.Errorf("NewestVersionInMinorOf(%v, %q) = %q, want %q", tc.offered, tc.like, got, tc.want)
			}
		})
	}
}

// TestNewestBuildOfPatchCatchesTheOtherDocumentedMisspelling — CheckVersion's
// near-miss rule normalizes away a leading `v`, so `1.34.6+lke2` is recognised.
// `v1.34.6`, with the `+lke` suffix simply left off, is not, and the runbook lists
// the two side by side as equally common.
func TestNewestBuildOfPatchCatchesTheOtherDocumentedMisspelling(t *testing.T) {
	catalog := []string{"v1.35.0+lke1", "v1.34.9+lke3", "v1.34.6+lke2", "v1.34.6+lke5"}
	for _, tc := range []struct{ like, want string }{
		{"v1.34.6", "v1.34.6+lke5"}, // the newest build OF THAT PATCH, not of that minor
		{"1.34.6", "v1.34.6+lke5"},  // v-less and build-less at once
		{"v1.34.9", "v1.34.9+lke3"},
		{"v1.33.6", ""},      // that patch is not offered at all
		{"v1.34.6+lke2", ""}, // already names a build; a different one would be an upgrade
		{"v1.34", ""},        // a minor is not a patch
		{"stable", ""},
		{"", ""},
	} {
		if got := NewestBuildOfPatch(catalog, tc.like); got != tc.want {
			t.Errorf("NewestBuildOfPatch(%q) = %q, want %q", tc.like, got, tc.want)
		}
	}
}

// TestDifferentMinorClaimsADifferenceOnlyWhenItCanSeeOne.
//
// PHRASED AS THE POSITIVE THE CALLER ACTS ON, because the negation is where this
// goes wrong. A `SameMinor` returning false for an unanswerable comparison reads
// correctly on its own — an unknown is not a match — and then inverts at the one
// call site, which warns on NOT-same: an unparseable version would have produced a
// confident "different MINOR" warning off a comparison nobody could make. The
// unknown-is-not-a-negative rule has to survive the caller's negation.
func TestDifferentMinorClaimsADifferenceOnlyWhenItCanSeeOne(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v1.34.6+lke2", "v1.33.6+lke7", true}, // a real difference
		{"v1.34.6+lke2", "v2.34.6+lke2", true}, // major counts
		{"v1.34.6+lke2", "v1.34.9+lke7", false},
		{"v1.34.6+lke2", "1.34", false}, // the reference need not be a build id
		{"v1.34", "v1.34.6", false},     // neither has to be
		// UNANSWERABLE IS NOT DIFFERENT. Each of these would have warned.
		{"v1.34.6+lke2", "stable", false},
		{"stable", "v1.34.6+lke2", false},
		{"v1.34.6+lke2", "v1.x", false},
		{"", "v1.34.6+lke2", false},
		{"v1.34.6+lke2", "", false},
		{"", "", false},
	} {
		if got := DifferentMinor(tc.a, tc.b); got != tc.want {
			t.Errorf("DifferentMinor(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
