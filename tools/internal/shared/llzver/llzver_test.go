package llzver

import (
	"reflect"
	"testing"
)

func TestLatestLLZTag(t *testing.T) {
	// Tags follow the shipped release line (v0.0.x) — the same shape the picker
	// meets on the real repo, so the numeric-vs-lexical trap is the live one:
	// a string sort would hand back v0.0.9.
	tags := []string{
		"llz-pool/v0.0.38", // module track (prefixed) — ignored
		"llz/v0.0.38",      // legacy CLI tag (prefixed) — ignored
		"llz/v0.0.40",      // legacy CLI tag (prefixed) — ignored
		"v0.0.2",
		"v0.0.10", // highest bare
		"v0.0.9",
		"v.0.0.30", // real typo tag on the repo — unparseable, ignored
		"vbroken",  // unparseable — ignored
	}
	got, ok := LatestLLZTag(tags)
	if !ok || got != "v0.0.10" {
		t.Errorf("LatestLLZTag = %q ok=%v, want v0.0.10", got, ok)
	}
	if _, ok := LatestLLZTag([]string{"llz-pool/v0.0.1", "llz/v0.0.99"}); ok {
		t.Error("expected no bare vX.Y.Z tag")
	}
}

// TestLatestLLZTagTieKeepsFirst pins the tie-break: Semver() ignores a -pre/+build
// tail, so two full releases can share one numeric core, and the picker replaces
// `best` only on a STRICTLY greater Version — keeping the FIRST of equals. gh
// lists newest first, so that is the newer release.
//
// This is not a detail: three shell implementations mirror this picker
// (install-llz.sh, llz-functional.sh, and the quickstart's by-hand snippets),
// and jq's natural spelling — sort_by(key) | last — silently does the OPPOSITE,
// because its sort is stable and the last of equals is the oldest-listed. Pin it
// here so the shells have something authoritative to agree with.
func TestLatestLLZTagTieKeepsFirst(t *testing.T) {
	// Newest-first, as `gh release list` returns them.
	if got, _ := LatestLLZTag([]string{"v1.2.3-hotfix", "v1.2.3"}); got != "v1.2.3-hotfix" {
		t.Errorf("tie: got %q, want the first-listed v1.2.3-hotfix", got)
	}
	if got, _ := LatestLLZTag([]string{"v1.2.3", "v1.2.3-hotfix"}); got != "v1.2.3" {
		t.Errorf("tie (reversed input): got %q, want the first-listed v1.2.3", got)
	}
}

func TestNormalizeLLZTag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"llz/v1.2.3", "v1.2.3"}, // legacy prefixed ref accepted, normalized to bare
		{"  v0.0.38 ", "v0.0.38"},
	} {
		if got := NormalizeLLZTag(tc.in); got != tc.want {
			t.Errorf("NormalizeLLZTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSemverAndLess(t *testing.T) {
	if _, _, _, ok := Semver("dev"); ok {
		t.Error("dev should not parse as Semver")
	}
	if _, _, _, ok := Semver("dev-abc123"); ok {
		t.Error("dev-<sha> should not parse as Semver")
	}
	if m, n, p, ok := Semver("llz/v1.2.3"); !ok || m != 1 || n != 2 || p != 3 {
		t.Errorf("Semver(llz/v1.2.3) = %d.%d.%d ok=%v", m, n, p, ok)
	}
	if m, _, _, ok := Semver("v2.0.0-rc1"); !ok || m != 2 {
		t.Errorf("Semver pre-release core: m=%d ok=%v", m, ok)
	}

	if !Less("v1.2.3", "v1.2.4") {
		t.Error("1.2.3 < 1.2.4")
	}
	if !Less("v1.9.0", "v1.10.0") {
		t.Error("1.9.0 < 1.10.0 (numeric, not lexical)")
	}
	if Less("v2.0.0", "v1.9.9") {
		t.Error("2.0.0 is not < 1.9.9")
	}
	// A dev build sorts below any real release, so self-update always proceeds.
	if !Less("dev", "v0.0.38") {
		t.Error("dev should sort below v0.0.38")
	}
}

func TestReleaseListArgv(t *testing.T) {
	if got := releaseListArgv("akamai-consulting/lke-landing-zone"); !reflect.DeepEqual(got,
		[]string{"gh", "release", "list", "--repo", "akamai-consulting/lke-landing-zone",
			"--limit", "200", "--json", "tagName,isDraft,isPrerelease"}) {
		t.Errorf("releaseListArgv: got %v", got)
	}
}
