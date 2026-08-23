package statepassphrase

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// passphraseWith builds a one-rune passphrase carrying r. A blank rune would trip
// the "passphrase is not set" guard before the character-class check ever runs, so
// those are sandwiched between two legal runes — the rejection then has to come
// from the class check, which is what is under test.
func passphraseWith(r rune) string {
	if strings.TrimSpace(string(r)) == "" {
		return "A" + string(r) + "A"
	}
	return string(r)
}

// classCase is one rune and whether the character class must accept it.
type classCase struct {
	r   rune
	ok  bool
	why string
}

// The passphrase is interpolated into an HCL string literal, so its character
// class is a security boundary: a single rune outside [A-Za-z0-9+/=_-] can close
// the string and append a `method "unencrypted"` block, which would write
// PLAINTEXT state. An off-by-one at either END of a range is the way that
// boundary silently moves, so every range endpoint AND both of its neighbours are
// pinned here rather than sampled from the middle.
func TestBuildNewKeyOnlyEncryptionPassphraseCharClassBoundaries(t *testing.T) {
	for _, tc := range []classCase{
		// Range endpoints — each must be INSIDE the class.
		{'A', true, "first upper-case"},
		{'Z', true, "last upper-case"},
		{'a', true, "first lower-case"},
		{'z', true, "last lower-case"},
		{'0', true, "first digit"},
		{'9', true, "last digit"},
		// Mid-range, so a range that collapses entirely is still caught.
		{'M', true, "mid upper-case"},
		{'m', true, "mid lower-case"},
		{'5', true, "mid digit"},
		// The explicitly enumerated base64/url runes.
		{'+', true, "base64 plus"},
		{'/', true, "base64 slash"},
		{'=', true, "base64 padding"},
		{'_', true, "url-safe underscore"},
		{'-', true, "url-safe hyphen"},
		// Immediate neighbours — each must be OUTSIDE the class. These are what a
		// widened range would wrongly admit.
		{'@', false, "the rune just below 'A'"},
		{'[', false, "the rune just above 'Z'"},
		{'^', false, "between the upper- and lower-case ranges"},
		{'`', false, "the rune just below 'a'"},
		{'{', false, "the rune just above 'z'"},
		{'.', false, "below '0'"},
		{'!', false, "well below '0'"},
		{':', false, "the rune just above '9'"},
		// The runes that actually break out of the HCL string.
		{'"', false, "a quote closes the HCL string"},
		{'\\', false, "a backslash escapes inside the HCL string"},
		{' ', false, "a space"},
		{'\t', false, "a tab"},
	} {
		_, err := buildNewKeyOnlyEncryption(passphraseWith(tc.r), "llz")
		if tc.ok && err != nil {
			t.Errorf("passphrase rune %q (%s) must be ACCEPTED, got: %v", tc.r, tc.why, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("passphrase rune %q (%s) must be REJECTED — it can be interpolated into the HCL encryption config", tc.r, tc.why)
		}
	}
}

// The key name becomes a bare HCL identifier in five places (key_provider, method,
// and three references). Its class is narrower than the passphrase's — no
// +/=- — because a hyphen alone makes the generated config unparseable, which
// fails the verify pass and would be read as "this root cannot be decrypted with
// the new key". Same endpoint-and-neighbour discipline.
func TestBuildNewKeyOnlyEncryptionKeyNameCharClassBoundaries(t *testing.T) {
	const goodPass = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for _, tc := range []classCase{
		{'A', true, "first upper-case"},
		{'Z', true, "last upper-case"},
		{'a', true, "first lower-case"},
		{'z', true, "last lower-case"},
		{'0', true, "first digit"},
		{'9', true, "last digit"},
		{'M', true, "mid upper-case"},
		{'m', true, "mid lower-case"},
		{'5', true, "mid digit"},
		{'_', true, "underscore is the only punctuation an HCL identifier allows"},
		{'@', false, "the rune just below 'A'"},
		{'[', false, "the rune just above 'Z'"},
		{'^', false, "between the upper- and lower-case ranges"},
		{'`', false, "the rune just below 'a'"},
		{'{', false, "the rune just above 'z'"},
		{'.', false, "below '0'"},
		{'!', false, "well below '0'"},
		{':', false, "the rune just above '9'"},
		{'-', false, "a hyphen is legal base64 but NOT an HCL identifier"},
		{'+', false, "plus"},
		{'/', false, "slash"},
		{'=', false, "equals"},
		{'"', false, "a quote closes the HCL string"},
		{' ', false, "a space"},
	} {
		_, err := buildNewKeyOnlyEncryption(goodPass, string(tc.r))
		if tc.ok && err != nil {
			t.Errorf("key-name rune %q (%s) must be ACCEPTED, got: %v", tc.r, tc.why, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("key-name rune %q (%s) must be REJECTED — it does not survive as a bare HCL identifier", tc.r, tc.why)
		}
	}
}

// The failure error is the operator's instruction on whether the old passphrase
// can be dropped, and BOTH of its numbers matter: "N of M failed" tells them how
// much of the instance is still readable only by the old key. An arithmetic slip
// in the denominator understates the blast radius of deleting it, so the exact
// sentence is pinned.
func TestRotateStatePassphraseIncompleteErrorCountsEveryRoot(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { return nil },
		func(d string) error {
			if strings.HasSuffix(d, "/databases") {
				return errors.New("decryption failed for all attempted")
			}
			return nil
		},
		allRoots())

	err := RunRotate(true, tmpRootsDir(t))
	if err == nil {
		t.Fatal("a root that fails verification MUST fail the command")
	}
	// 4 roots present: 3 verified, 1 failed. The denominator is verified+failed,
	// i.e. every root that was actually attempted.
	const want = "state-passphrase rollover incomplete: 1 of 4 root(s) failed — old passphrase MUST be retained"
	if err.Error() != want {
		t.Errorf("the failure count must be exact.\n got: %s\nwant: %s", err.Error(), want)
	}
}

// The success summary is what licenses deleting TF_STATE_ENCRYPTION_PASSPHRASE_OLD.
// Its two counts have to be right: "verified" is how many roots were proved
// readable with the new key ALONE, and "skipped" is how many were never attempted.
// A wrong count here reads as coverage the rollover does not have.
func TestRotateStatePassphraseSummaryCountsVerifiedAndSkipped(t *testing.T) {
	rotationWindowEnv(t)
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)

	present := allRoots()
	delete(present, "databases")
	withRolloverSeams(t,
		func(string) error { return nil },
		func(string) error { return nil },
		present)

	if err := RunRotate(true, tmpRootsDir(t)); err != nil {
		t.Fatalf("rollover: %v", err)
	}
	got, readErr := os.ReadFile(sum)
	if readErr != nil {
		t.Fatalf("read step summary: %v", readErr)
	}
	// The counts, AND the withheld licence. A partially-skipped rollover used to
	// print "can now be deleted" — `verified == 0` catches only the all-skipped
	// run, and partial is the shape an instance actually reaches, because "root
	// not present" is decided by a stat over a render-time gitignored directory.
	for _, want := range []string{
		"3 root(s) verified with the new passphrase alone, 1 SKIPPED",
		"databases",
		"Do not delete `TF_STATE_ENCRYPTION_PASSPHRASE_OLD` yet",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("step summary must state the exact counts and withhold the licence.\nwant substring: %s\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "can now be deleted") {
		t.Errorf("a rollover that skipped a root must NOT license deleting the old passphrase:\n%s", got)
	}
}

// AND A COMPLETE ONE STILL GRANTS IT, or the rollover can never be finished and
// the old passphrase accumulates forever.
func TestRotateStatePassphraseLicensesDeletionWhenNothingWasSkipped(t *testing.T) {
	rotationWindowEnv(t)
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	withRolloverSeams(t,
		func(string) error { return nil },
		func(string) error { return nil },
		allRoots())

	if err := RunRotate(true, tmpRootsDir(t)); err != nil {
		t.Fatalf("rollover: %v", err)
	}
	got, err := os.ReadFile(sum)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "can now be deleted") {
		t.Errorf("a rollover with every root re-keyed must license the deletion:\n%s", got)
	}
}

// Roots are reported in a stable, sorted order rather than in the order the
// package var happens to list them (vpc, cluster, object-storage, databases). An
// operator diffing two rollover summaries — the usual way to spot which root
// regressed between runs — gets noise instead of signal if the order drifts.
func TestRotateStatePassphraseReportsRootsInSortedOrder(t *testing.T) {
	rotationWindowEnv(t)
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)

	withRolloverSeams(t,
		func(string) error { return nil },
		func(string) error { return nil },
		allRoots())

	if err := RunRotate(true, tmpRootsDir(t)); err != nil {
		t.Fatalf("rollover: %v", err)
	}
	got, readErr := os.ReadFile(sum)
	if readErr != nil {
		t.Fatalf("read step summary: %v", readErr)
	}
	// Sorted, NOT the declaration order of statePassphraseRoots.
	want := []string{"`cluster`", "`databases`", "`object-storage`", "`vpc`"}
	prev := -1
	for _, root := range want {
		at := strings.Index(string(got), root)
		if at < 0 {
			t.Fatalf("root %s missing from the summary:\n%s", root, got)
		}
		if at <= prev {
			t.Errorf("roots must be reported in sorted order %v, got:\n%s", want, got)
		}
		prev = at
	}
}

// The trim threshold: a message of exactly n lines is short enough to keep whole.
func TestLastLinesAtTheThreshold(t *testing.T) {
	if got := lastLines("a\nb\nc", 3); got != "a\nb\nc" {
		t.Errorf("exactly n lines should pass through whole, got %q", got)
	}
	if got := lastLines("a\nb\nc\nd", 3); got != "b\nc\nd" {
		t.Errorf("n+1 lines should be trimmed to the last n, got %q", got)
	}
}
