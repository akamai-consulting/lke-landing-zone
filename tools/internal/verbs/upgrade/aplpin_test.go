package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// The behavior, stated once: a pin llz set is removed so the environment tracks
// the baseline; a pin the operator set survives untouched.
func TestDropTrackingPin(t *testing.T) {
	const shape = "spec:\n  cluster:\n    bootstrap:\n      name: platform-prod\n%s      aplValues:\n        repoURL: https://example.invalid/x.git\n"
	line := func(v string) string { return "      aplChartVersion: " + v + "\n" }

	cases := []struct {
		name    string
		pin     string
		dropped bool
	}{
		// The case this whole lever exists for: the baseline the PREVIOUS release
		// targeted, left behind by a bump.
		{"the previous baseline", "v6.2.0", true},
		{"the previous baseline, bare", "6.2.0", true},
		{"an older baseline", "v6.1.0", true},
		{"the original baseline", "6.0.0", true},
		// Redundant rather than stale, but it says nothing the default does not and
		// would go stale on the NEXT bump. Removing it now is what makes this the
		// last upgrade that has to touch the field.
		{"the current baseline", clusterspec.BaselineAplChartVersion, true},
		// Never ours. Dropping these would move an environment its owner is holding.
		{"a deliberate patch pin", "6.0.1", false},
		{"a deliberate hold ahead", "6.3.0", false},
		// llz has never targeted an rc, so riding one is an operator's choice.
		{"a release candidate of the baseline", clusterspec.BaselineAplChartVersion + "-rc.1", false},
		{"an unparseable pin", "latest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.Replace(shape, "%s", line(tc.pin), 1)
			out, dropped, refused := dropTrackingPin(in)
			if refused {
				t.Fatalf("a single well-formed pin must not be refused")
			}
			if (dropped != "") != tc.dropped {
				t.Fatalf("dropped = %q, want dropped=%v for pin %q", dropped, tc.dropped, tc.pin)
			}
			if tc.dropped {
				if strings.Contains(out, "aplChartVersion") {
					t.Errorf("the pin should be gone:\n%s", out)
				}
				// The rest of the file is the operator's and must survive intact.
				for _, keep := range []string{"name: platform-prod", "repoURL: https://example.invalid/x.git"} {
					if !strings.Contains(out, keep) {
						t.Errorf("removing the pin must not disturb %q:\n%s", keep, out)
					}
				}
			} else if out != in {
				t.Errorf("a pin llz never set must survive byte-identical:\ngot:\n%s\nwant:\n%s", out, in)
			}
		})
	}
}

// The delivered example carries a COMMENTED pin, and it is what a reader copies
// from. An upgrade that "helpfully" deleted the commented line would strip the
// documentation of the field from every instance.
func TestDropTrackingPinIgnoresComments(t *testing.T) {
	in := "    bootstrap:\n      # aplChartVersion: v6.2.0   # optional; omit to track the llz baseline.\n      name: platform-prod\n"
	out, dropped, refused := dropTrackingPin(in)
	if dropped != "" || refused || out != in {
		t.Errorf("a commented pin is documentation, not a pin — got dropped=%q refused=%v, content changed=%v", dropped, refused, out != in)
	}
}

// FAIL CLOSED. Two active keys is a file shape this rule was not written against,
// and editing the wrong one of two pins is worse than editing neither.
func TestDropTrackingPinRefusesAmbiguity(t *testing.T) {
	in := "a:\n      aplChartVersion: v6.2.0\nb:\n      aplChartVersion: v6.1.0\n"
	out, pin, refused := dropTrackingPin(in)
	if !refused {
		t.Fatal("two active aplChartVersion keys must be refused, not guessed at")
	}
	if out != in {
		t.Error("a refused file must be left byte-identical")
	}
	if pin == "" {
		t.Error("the refusal should still name a pin it saw, so the operator can find the file")
	}
}

// Inline comments and quotes are ordinary YAML and must not defeat the match —
// otherwise the pin silently survives and the drift warning never stops.
func TestDropTrackingPinValueForms(t *testing.T) {
	for _, in := range []string{
		"      aplChartVersion: v6.2.0   # pinned during the 6.2 rollout\n",
		"      aplChartVersion: \"v6.2.0\"\n",
		"      aplChartVersion: 'v6.2.0'\n",
	} {
		if _, dropped, _ := dropTrackingPin(in); dropped == "" {
			t.Errorf("should have recognised the pin in %q", in)
		}
	}

	// TAB INDENTATION IS NOT YAML. The old regex accepted it; the parser rejects the
	// document outright, and a spec that does not parse is refused rather than
	// edited — an unreadable file is not an absent pin.
	if _, _, refused := dropTrackingPin("\taplChartVersion: v6.2.0\n"); !refused {
		t.Error("a document that does not parse must be refused, not silently skipped")
	}
}

// writeEnv is a minimal env spec carrying one pin.
func writeEnv(t *testing.T, dir, name, pin string) {
	t.Helper()
	body := "name: " + name + "\ncluster:\n  bootstrap:\n    name: platform-" + name + "\n"
	if pin != "" {
		body += "    aplChartVersion: " + pin + "\n"
	}
	writeInstanceFile(t, dir, filepath.Join("environments", name+".yaml"), body)
}

func TestSweepAplPins(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "prod", "v6.2.0") // ours → dropped
	writeEnv(t, dir, "dev", "6.0.1")   // theirs → kept
	writeEnv(t, dir, "lab", "")        // nothing to do
	// The delivered reference material. It is template-managed and its whole job is
	// to SHOW the field, so it must never be edited.
	writeInstanceFile(t, dir, filepath.Join("environments", "prod-web-ord.yaml.example"),
		"cluster:\n  bootstrap:\n    aplChartVersion: v6.2.0\n")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 1 || res.Dropped[0].File != filepath.Join("environments", "prod.yaml") {
		t.Errorf("want exactly prod.yaml dropped, got %+v", res.Dropped)
	}
	if len(res.Kept) != 1 || res.Kept[0].Pin != "6.0.1" {
		t.Errorf("want dev's deliberate 6.0.1 reported as kept, got %+v", res.Kept)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "environments", "prod.yaml"))
	if strings.Contains(string(b), "aplChartVersion") {
		t.Errorf("prod.yaml should no longer pin:\n%s", b)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "environments", "dev.yaml"))
	if !strings.Contains(string(b), "aplChartVersion: 6.0.1") {
		t.Errorf("dev.yaml's deliberate pin must survive:\n%s", b)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "environments", "prod-web-ord.yaml.example"))
	if !strings.Contains(string(b), "aplChartVersion: v6.2.0") {
		t.Errorf("the .yaml.example is template reference material and must be untouched:\n%s", b)
	}
}

// --dry-run must decide the same thing and write nothing. A dry run that edited
// the tree would be the one failure mode the flag exists to make impossible.
func TestSweepAplPinsDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "prod", "v6.2.0")

	res, err := sweepAplPins(dir, false)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 1 {
		t.Fatalf("a dry run must still REPORT the drop, got %+v", res.Dropped)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "environments", "prod.yaml"))
	if !strings.Contains(string(b), "aplChartVersion: v6.2.0") {
		t.Errorf("a dry run must not write:\n%s", b)
	}
}

// After the sweep the environment must RESOLVE to the baseline — the point of
// removing the pin, and the thing a test of the file contents alone does not say.
// Calls the real resolver rather than restating what an absent pin means.
func TestDroppedPinResolvesToTheBaseline(t *testing.T) {
	if got := clusterspec.EffectiveAplChartVersion(""); got != clusterspec.BaselineAplChartVersion {
		t.Fatalf("an omitted pin resolves to %q, want the baseline %q — without this the lever would silently un-pin an environment onto nothing", got, clusterspec.BaselineAplChartVersion)
	}
	if clusterspec.AplChartDriftOf("") != clusterspec.AplChartDriftNone {
		t.Error("an omitted pin must read as no drift, or removing the pin would trade a stale warning for a permanent one")
	}
}

// PINS BOTH CALL SITES AND THEIR ORDER, which no unit test of the sweep can see.
// The lever has two halves on purpose:
//
//   - the PREVIEW, above Run's dry-run return, because a dry run is the one where
//     the operator can still change their mind. Sited below it, the whole lever was
//     unreachable on --dry-run and the test asserting a dry run writes nothing was
//     exercising a path production never took.
//   - the WRITE, below the conflict-marker gate, because that gate and the answers
//     gate both `return err`. Writing to the operator's owned spec files above them
//     left an aborted upgrade carrying a mutation its own failure message never
//     mentioned.
//
// And the write must precede printSummary — `git diff --stat` over the whole tree,
// the one place an operator sees what the upgrade touched. NOT renderAfter: nothing
// in the render path reads the pin (EffectiveAplChartVersion has no non-test callers,
// and `apl_chart_version` is no longer a tfvar).
func TestUpgradeRunRetracksPinsBeforeRendering(t *testing.T) {
	b, err := os.ReadFile("upgrade.go")
	if err != nil {
		t.Fatalf("read upgrade.go: %v", err)
	}
	src := string(b)

	idx := func(what, needle string) int {
		i := strings.Index(src, needle)
		if i < 0 {
			t.Fatalf("%s (%q) not found — this test's ordering claims can no longer be checked, which is a failure, not a pass", what, needle)
		}
		return i
	}
	write := idx("the write call", "retrackAplPins(false)")
	conflict := idx("the conflict-marker gate", "resolve the conflict marker(s) above")
	summary := idx("the upgrade summary", "printSummary(oldRef, newRef)")

	// The preview lives INSIDE the dry-run branch, ahead of its return, and its
	// result reaches the checklist. Asserted on the CALL, not the branch's exact text —
	// a test pinned to formatting breaks on reformatting and proves nothing.
	preview := idx("the dry-run preview call", "printNextSteps(retrackAplPins(true))")
	dryBranch := idx("the dry-run branch", "if dryRun {")
	if preview < dryBranch {
		t.Error("the preview must sit inside Run's dry-run branch, or `llz upgrade --dry-run` never shows the pin removal")
	}
	if preview > write {
		t.Error("the dry-run preview must precede the write half — it is the branch that returns early")
	}
	if write < conflict {
		t.Error("the write must run AFTER the conflict-marker gate, or an aborted upgrade leaves a spec mutation it never reported")
	}
	if write > summary {
		t.Error("the write must run BEFORE printSummary, or the operator's one diffstat of the upgrade silently omits the spec edit")
	}
}

// CRLF. Go's `$` in multiline mode matches only before a \n, so a naive anchor
// misses a pin in a CRLF-terminated spec entirely — and a missed pin lands in
// NONE of Dropped/Kept/Refused. It does not warn, it does not refuse, it simply
// does not exist, which is precisely the silence the Refused arm was written to
// make impossible.
func TestDropTrackingPinHandlesCRLF(t *testing.T) {
	in := "spec:\r\n  cluster:\r\n    bootstrap:\r\n      aplChartVersion: v6.2.0\r\n      name: platform-prod\r\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused {
		t.Fatal("a CRLF file is not ambiguous")
	}
	if dropped != "v6.2.0" {
		t.Fatalf("dropped = %q, want v6.2.0 — a CRLF spec must not silently escape the sweep", dropped)
	}
	if strings.Contains(out, "aplChartVersion") {
		t.Errorf("the pin should be gone:\n%q", out)
	}
	if !strings.Contains(out, "name: platform-prod") {
		t.Errorf("the rest of the CRLF file must survive:\n%q", out)
	}
}

// THE OTHER ALTITUDE. `landingzone.yaml` carries spec.defaults.cluster.bootstrap,
// which clusterspec's inheritance (mergeCluster in merge.go) folds into EVERY environment — so a pin there
// is inherited by every deployment. Sweeping only environments/*.yaml left the
// single most economical place to put one warning forever, in a file this lever
// never opened.
func TestSweepAplPinsCoversTheSpecRoot(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: v6.2.0\n        managedAppPlatform: true\n")
	// The delivered example sits right beside it and must not be touched.
	writeInstanceFile(t, dir, "landingzone.yaml.example",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: v6.2.0\n")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 1 || res.Dropped[0].File != "landingzone.yaml" {
		t.Fatalf("want landingzone.yaml dropped, got %+v", res.Dropped)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "landingzone.yaml"))
	if strings.Contains(string(b), "aplChartVersion") {
		t.Errorf("the inherited default pin should be gone:\n%s", b)
	}
	if !strings.Contains(string(b), "managedAppPlatform: true") {
		t.Errorf("the rest of spec.defaults must survive:\n%s", b)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "landingzone.yaml.example"))
	if !strings.Contains(string(b), "aplChartVersion: v6.2.0") {
		t.Errorf("landingzone.yaml.example is template reference material:\n%s", b)
	}
}

// The seam itself, not just the sweep under it: the guard for a pre-spec
// instance, and the NEXT STEPS entry it produces. That entry is the only thing an
// operator sees when the upgrade deliberately DID NOT touch a pin, so an empty
// string here is the difference between "we left your pin alone" and silence.
func TestRetrackAplPinsReportsOnlyWhatItLeftBehind(t *testing.T) {
	t.Run("nothing to say when every pin was ours", func(t *testing.T) {
		dir := newRenderableInstance(t, "v0.4.0")
		writeEnv(t, dir, "prod", "v6.2.0")
		t.Chdir(dir)

		if step := retrackAplPins(false); len(step) != 0 {
			t.Errorf("a clean sweep must contribute NO checklist entry — a checklist that always prints stops being read; got %q", step)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "environments", "prod.yaml"))
		if strings.Contains(string(b), "aplChartVersion") {
			t.Errorf("the pin should have been dropped:\n%s", b)
		}
	})

	t.Run("names the file whose pin it refused to touch", func(t *testing.T) {
		dir := newRenderableInstance(t, "v0.4.0")
		writeEnv(t, dir, "prod", "6.0.1")
		t.Chdir(dir)

		step := strings.Join(retrackAplPins(false), "\n")
		if !strings.Contains(step, "prod.yaml") {
			t.Errorf("the checklist entry must name the file the operator has to look at, got %q", step)
		}
	})

	t.Run("a pre-spec instance is not an error", func(t *testing.T) {
		dir := t.TempDir()
		writeInstanceFile(t, dir, ".copier-answers.yml", "llz_version: v0.4.0\n")
		t.Chdir(dir)

		if step := retrackAplPins(false); len(step) != 0 {
			t.Errorf("an instance that never adopted the spec has no pins to re-track, got %q", step)
		}
	})
}

// THE EXCLUSION IS LOAD-BEARING, asserted directly. Globbing `*.yaml` could never
// match `*.yaml.example`, so the filter was unreachable and the two tests claiming
// the delivered examples survive an upgrade passed having examined nothing — they
// would have passed with the protection deleted. This asserts the candidate set is
// wide enough for the filter to have work to do, and that it does it.
func TestEnvSpecFilesExcludesTheDeliveredExamples(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"landingzone.yaml", "landingzone.yaml.example",
		"environments/prod.yaml", "environments/prod-web-ord.yaml.example",
		"environments/prod.yaml.bak",
	} {
		writeInstanceFile(t, dir, rel, "spec: {}\n")
	}

	got, err := envSpecFiles(dir)
	if err != nil {
		t.Fatalf("envSpecFiles: %v", err)
	}
	var rels []string
	for _, f := range got {
		r, _ := filepath.Rel(dir, f)
		rels = append(rels, filepath.ToSlash(r))
	}
	want := "environments/prod.yaml,landingzone.yaml"
	if strings.Join(rels, ",") != want {
		t.Errorf("envSpecFiles = %v, want exactly [%s] — an example or a stray backup would be edited", rels, want)
	}
}

// A SWEEP THAT FAILS HALFWAY HAS ALREADY WRITTEN. Returning early on the error
// discarded the partial result, so files the sweep had rewritten sat changed in the
// operator's tree and in no report — an upgrade silently editing files it never
// mentioned.
func TestSweepAplPinsReportsWhatItWroteBeforeFailing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so this cannot be provoked")
	}
	dir := t.TempDir()
	writeEnv(t, dir, "aaa", "v6.2.0") // sorts first, gets rewritten
	writeEnv(t, dir, "zzz", "v6.2.0") // sorts last, made unreadable
	if err := os.Chmod(filepath.Join(dir, "environments", "zzz.yaml"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "environments", "zzz.yaml"), 0o600) })

	res, err := sweepAplPins(dir, true)
	if err == nil {
		t.Fatal("an unreadable spec must be an error — 'could not read' is not 'nothing to do'")
	}
	if len(res.Dropped) != 1 || res.Dropped[0].File != filepath.Join("environments", "aaa.yaml") {
		t.Fatalf("the partial result must still name what was already written, got %+v", res.Dropped)
	}

}

// AND THE OPERATOR MUST BE TOLD. The partial result is worth nothing if the caller
// drops it on the error path — which it did: the rewritten file sat changed in the
// tree and in no report, an upgrade silently editing a file it never mentioned.
// Asserted on the STREAM the operator actually reads, not on the returned value.
func TestRetrackAplPinsNamesFilesItAlreadyRewrote(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable, so this cannot be provoked")
	}
	dir := newRenderableInstance(t, "v0.4.0")
	writeEnv(t, dir, "aaa", "v6.2.0")
	writeEnv(t, dir, "zzz", "v6.2.0")
	unreadable := filepath.Join(dir, "environments", "zzz.yaml")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	t.Chdir(dir)

	out := captureStderr(t, func() { retrackAplPins(false) })
	if !strings.Contains(out, "aaa.yaml") {
		t.Errorf("the file already rewritten must be named before the error, got:\n%s", out)
	}
	if !strings.Contains(out, "already rewritten") {
		t.Errorf("the report must say the sweep stopped partway, or the operator reads a clean success, got:\n%s", out)
	}
}

// KEPT AND REFUSED ARE DIFFERENT PROBLEMS WITH DIFFERENT REMEDIES. One line
// covering both told the operator a refused file "is not one llz set" — a claim
// about the pin's value, when the real problem is that the file carries two of them.
func TestRetrackAplPinsSeparatesKeptFromRefused(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	writeEnv(t, dir, "held", "6.0.1")
	writeInstanceFile(t, dir, filepath.Join("environments", "dup.yaml"),
		"cluster:\n  bootstrap:\n    aplChartVersion: v6.2.0\n  other:\n    aplChartVersion: v6.1.0\n")
	t.Chdir(dir)

	steps := retrackAplPins(false)

	// SEPARATE ENTRIES, not one sentence. printNextSteps numbers what it is given, so
	// joining these with "; " put "you may want to look at this" and "fix this or
	// validation blocks" on one line — undoing the whole reason they are tracked
	// apart. Asserted per entry, because a Contains() over the joined text passes
	// either way and would not have noticed.
	if len(steps) != 2 {
		t.Fatalf("want two checklist entries (one kept, one refused), got %d: %q", len(steps), steps)
	}
	var sawKept, sawRefused bool
	for _, st := range steps {
		switch {
		case strings.Contains(st, "held.yaml"):
			sawKept = true
			if !strings.Contains(st, "not one llz set") {
				t.Errorf("the kept entry must give the kept reason, got %q", st)
			}
			if strings.Contains(st, "dup.yaml") {
				t.Errorf("the two files must not share an entry, got %q", st)
			}
		case strings.Contains(st, "dup.yaml"):
			sawRefused = true
			// THE REASON THAT APPLIES, not a menu of candidates. The old wording
			// listed "more than one key, or a flow-style mapping" — and a flow
			// mapping cannot refuse at all now, while the causes that do refuse
			// went unnamed.
			if !strings.Contains(st, "more than one aplChartVersion key") {
				t.Errorf("the refused entry must name the actual cause, got %q", st)
			}
		default:
			t.Errorf("unexpected checklist entry %q", st)
		}
	}
	if !sawKept || !sawRefused {
		t.Errorf("both remedies must appear, got %q", steps)
	}
}

// TENSE. A dry run writes nothing, so a past-tense "no longer pins" tells the
// operator an edit happened that did not — and a dry run is precisely the moment
// they are deciding whether to let it. reportClobberedManaged beside it gets this
// right for the same reason.
func TestRetrackAplPinsTensesTheDryRun(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	writeEnv(t, dir, "prod", "v6.2.0")
	t.Chdir(dir)

	dry := captureStderr(t, func() { retrackAplPins(true) })
	if !strings.Contains(dry, "would stop pinning") {
		t.Errorf("a dry run must speak in the conditional, got:\n%s", dry)
	}
	if strings.Contains(dry, "no longer pins") {
		t.Errorf("a dry run must not claim an edit it did not make, got:\n%s", dry)
	}

	wet := captureStderr(t, func() { retrackAplPins(false) })
	if !strings.Contains(wet, "no longer pins") {
		t.Errorf("a real run must state what it did, got:\n%s", wet)
	}
}

// SINGLE-LINE FLOW IS EDITED, NOT REFUSED — and it has to be, because it is the
// shape LLZ ITSELF SCAFFOLDS. envdef.EnsureLandingZone writes
// `bootstrap: { managedAppPlatform: true }`, so an operator pinning at the
// spec.defaults altitude naturally lands on
// `bootstrap: { managedAppPlatform: true, aplChartVersion: v6.2.0 }`. Refusing that
// meant every upgrade nagged forever about a shape this repo authored and the pin
// never retired.
//
// The entry AND one separator go, so the mapping stays well-formed whether the pin
// was first, last, or the only member — and the operator's spacing survives, because
// a gratuitous reformat of someone else's file inside a diff they must review is its
// own small cost.
func TestDropTrackingPinEditsSingleLineFlow(t *testing.T) {
	cases := map[string]string{
		"      bootstrap: { managedAppPlatform: true, aplChartVersion: v6.2.0 }\n": "      bootstrap: { managedAppPlatform: true }\n",
		"      bootstrap: { aplChartVersion: v6.2.0, managedAppPlatform: true }\n": "      bootstrap: { managedAppPlatform: true }\n",
		"      bootstrap: { a: 1, aplChartVersion: v6.2.0, b: 2 }\n":               "      bootstrap: { a: 1, b: 2 }\n",
		"      bootstrap: {aplChartVersion: v6.2.0}\n":                             "      bootstrap: {}\n",
		"cluster: {bootstrap: {aplChartVersion: 6.2.0}}\n":                         "cluster: {bootstrap: {}}\n",
	}
	for in, want := range cases {
		out, dropped, refused := dropTrackingPin(in)
		if refused {
			t.Errorf("a single-line flow mapping is editable, not ambiguous: %q", in)
			continue
		}
		if dropped == "" {
			t.Errorf("should have found the pin in %q", in)
			continue
		}
		if out != want {
			t.Errorf("dropFlowEntry(%q)\n got %q\nwant %q", in, out, want)
		}
	}

	// A pin llz never set stays, in flow style as much as in block style.
	theirs := "      bootstrap: { managedAppPlatform: true, aplChartVersion: 6.0.1 }\n"
	if out, dropped, refused := dropTrackingPin(theirs); dropped != "" || refused || out != theirs {
		t.Errorf("a deliberate flow-style pin must survive byte-identical, got dropped=%q refused=%v out=%q", dropped, refused, out)
	}

	// ...and a COMMENTED flow-style mention is still documentation, not a pin.
	doc := "    # bootstrap: {aplChartVersion: v6.2.0}   # example\n    name: x\n"
	if _, dropped, refused := dropTrackingPin(doc); refused || dropped != "" {
		t.Errorf("a commented example must not be edited or refused, got dropped=%q refused=%v", dropped, refused)
	}
}

// A FAILED WRITE MUST NOT BE REPORTED AS A DROP. res.Dropped was appended before
// os.WriteFile, so a read-only spec produced the green "no longer pins apl-core X"
// for a file that was unchanged and still pinning — the report stating the exact
// opposite of the tree. The read-failure arm above did not cover this; it is the
// same bug one line further on.
func TestSweepAplPinsDoesNotReportAFailedWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only file is still writable, so this cannot be provoked")
	}
	dir := t.TempDir()
	writeEnv(t, dir, "prod", "v6.2.0")
	f := filepath.Join(dir, "environments", "prod.yaml")
	if err := os.Chmod(f, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(f, 0o600) })

	res, err := sweepAplPins(dir, true)
	if err == nil {
		t.Fatal("an unwritable spec must be an error")
	}
	if len(res.Dropped) != 0 {
		t.Errorf("a file that could not be written must NOT be reported as dropped, got %+v", res.Dropped)
	}
	b, _ := os.ReadFile(f)
	if !strings.Contains(string(b), "aplChartVersion: v6.2.0") {
		t.Errorf("the file must be unchanged:\n%s", b)
	}
}

// A TRAILING COMMENT IS A COMMENT. `aplChartVersion: v6.2.0   # was
// aplChartVersion: v6.1.0` is one active key and a note about the other; counting
// the note made the file look ambiguous, so the sweep refused it, the stale pin
// survives, and the printed remedy describes a problem the file does not have.
func TestDropTrackingPinIgnoresTrailingCommentMentions(t *testing.T) {
	in := "    bootstrap:\n      aplChartVersion: v6.2.0   # was aplChartVersion: v6.1.0\n      name: p\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused {
		t.Fatal("a note about another version in a trailing comment is not a second key")
	}
	if dropped != "v6.2.0" {
		t.Fatalf("dropped = %q, want v6.2.0", dropped)
	}
	if strings.Contains(out, "aplChartVersion") {
		t.Errorf("the pin and its trailing note go together:\n%s", out)
	}
}

// AN UNPARSEABLE PIN IS NOT A DELIBERATE CHOICE. The sweep leaves it alone for the
// same reason as any non-baseline value, but clusterspec HARD-BLOCKS that spec — so
// calling it "deliberate ... left alone" sent the operator away reassured about a
// file that fails the next command they run.
func TestRetrackAplPinsFlagsAnUnparseablePin(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	writeEnv(t, dir, "broken", "latest")
	t.Chdir(dir)

	var steps []string
	out := captureStderr(t, func() { steps = retrackAplPins(false) })
	step := strings.Join(steps, "\n")
	if !strings.Contains(out, "BLOCK") {
		t.Errorf("an unparseable pin must be named as blocking, not as a deliberate choice, got:\n%s", out)
	}
	if strings.Contains(out, "deliberately") {
		t.Errorf("%q is not a deliberate version choice, got:\n%s", "latest", out)
	}
	// THE CHECKLIST, not only the stream. The stderr line scrolls; printNextSteps is
	// the last screen of the upgrade, and it described a spec that hard-blocks as an
	// optional "review" because the pin was folded in with the deliberate ones. The
	// Asserting stderr alone would miss it: that line scrolls, the checklist does not.
	if !strings.Contains(step, "BLOCK") || !strings.Contains(step, "broken.yaml") {
		t.Errorf("the checklist must name the blocking file and say it blocks, got %q", step)
	}
	if strings.Contains(step, "it is not one llz set") {
		t.Errorf("a blocking pin must not be filed under the deliberate-choice remedy, got %q", step)
	}
}

// A QUOTED SCALAR MENTIONING THE KEY IS NOT A SECOND KEY. The mention counter was
// an unanchored substring match, so a spec carrying `note: "see aplChartVersion:
// docs"` beside one perfectly editable pin was counted as two, refused, and handed
// a remedy for a shape it does not have. It fails SAFE — the pin survives rather than
// being mangled — but the lever silently stops working on that file.
func TestDropTrackingPinAnchorsTheMentionCount(t *testing.T) {
	in := "    bootstrap:\n      aplChartVersion: v6.2.0\n      note: \"see aplChartVersion: docs\"\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused {
		t.Fatal("a mention inside a quoted scalar is not a YAML key")
	}
	if dropped != "v6.2.0" {
		t.Fatalf("dropped = %q, want v6.2.0", dropped)
	}
	if !strings.Contains(out, "see aplChartVersion: docs") {
		t.Errorf("the operator's own text must survive:\n%s", out)
	}
	// The shapes that ARE two keys still refuse — anchoring must not blind it.
	for _, dup := range []string{
		"a:\n      aplChartVersion: v6.2.0\nb:\n      aplChartVersion: v6.1.0\n",
		// Only genuine duplication refuses now — the parser edits every flow shape.
		"a: { aplChartVersion: v6.2.0 }\nb: { aplChartVersion: v6.1.0 }\n",
	} {
		if _, _, r := dropTrackingPin(dup); !r {
			t.Errorf("must still refuse: %q", dup)
		}
	}
}

// A MULTI-LINE FLOW MAPPING IS EDITED, NOT REFUSED: the parser says which mapping the
// key belongs to, so the entry and one separator go and no sibling is left dangling.
func TestDropTrackingPinEditsMultiLineFlow(t *testing.T) {
	for in, want := range map[string]string{
		"cluster:\n  bootstrap: {\n    name: p,\n    aplChartVersion: v6.2.0\n  }\n": "cluster:\n  bootstrap: {\n    name: p\n  }\n",
		"cluster:\n  bootstrap: {\n    aplChartVersion: v6.2.0,\n    name: p\n  }\n": "cluster:\n  bootstrap: {\n    name: p\n  }\n",
	} {
		out, dropped, refused := dropTrackingPin(in)
		if refused || dropped != "v6.2.0" {
			t.Errorf("%q: dropped=%q refused=%v", in, dropped, refused)
			continue
		}
		if out != want {
			t.Errorf("multi-line flow\n got %q\nwant %q", out, want)
		}
	}
}

// A COMMA IN THE COMMENT IS PROSE, NOT FLOW PUNCTUATION. Reading the raw capture
// refuses this file on the strength of a comma inside its own comment, leaving the
// stale pin in place and printing a remedy for a shape it does not have.
func TestDropTrackingPinIgnoresCommasInComments(t *testing.T) {
	in := "      aplChartVersion: v6.2.0   # pinned during the 6.2 rollout, revisit at 6.3\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused {
		t.Fatal("a comma in a trailing comment is prose, not a flow mapping")
	}
	if dropped != "v6.2.0" {
		t.Fatalf("dropped = %q, want v6.2.0", dropped)
	}
	if strings.Contains(out, "aplChartVersion") {
		t.Errorf("the pin should be gone:\n%q", out)
	}
}

// A CLOSED FLOW MAPPING ELSEWHERE MUST NOT AFFECT A LATER BLOCK-STYLE PIN. The
// hand-rolled brace counter had to be taught this (and had a clamp bug doing it);
// the parser simply knows which mapping a key belongs to.
func TestFlowMappingElsewhereDoesNotAffectABlockPin(t *testing.T) {
	in := "cluster:\n  labels: {a: 1, b: 2}\n  bootstrap:\n    aplChartVersion: v6.2.0\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused || dropped != "v6.2.0" {
		t.Fatalf("dropped=%q refused=%v", dropped, refused)
	}
	if !strings.Contains(out, "labels: {a: 1, b: 2}") {
		t.Errorf("the unrelated flow mapping must be untouched:\n%q", out)
	}
	if strings.Contains(out, "aplChartVersion") {
		t.Errorf("the pin should be gone:\n%q", out)
	}
}

// A STRAY `}` IN A SCALAR IS JUST TEXT. The hand-rolled brace counter banked
// negative depth on it and cancelled a real `{`, which took a clamp to fix; the
// parser never counted braces in the first place.
func TestStrayBraceInAScalarIsHarmless(t *testing.T) {
	in := "spec:\n  a: \"}\"\n  bootstrap: {\n    name: p,\n    aplChartVersion: v6.2.0\n  }\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused || dropped != "v6.2.0" {
		t.Fatalf("dropped=%q refused=%v", dropped, refused)
	}
	if !strings.Contains(out, `a: "}"`) {
		t.Errorf("the operator's scalar must survive:\n%q", out)
	}
}

// A BLOCK SCALAR'S BODY IS TEXT, NOT KEYS — and to the parser it is not a key at
// all, so this is a NO-OP rather than a refusal. That is the stronger answer: the
// hand-rolled version had to detect the block, and a refusal still nagged the
// operator about a file with nothing wrong with it.
func TestDropTrackingPinIgnoresBlockScalarBodies(t *testing.T) {
	for _, in := range []string{
		"spec:\n  notes: |\n    aplChartVersion: v6.2.0\n",
		"spec:\n  notes: |\n    bootstrap: { aplChartVersion: v6.2.0 }\n",
	} {
		out, dropped, refused := dropTrackingPin(in)
		if refused || dropped != "" || out != in {
			t.Errorf("%q: prose is not a pin, got dropped=%q refused=%v", in, dropped, refused)
		}
		if got := foundAplPin(in); got != "" {
			t.Errorf("%q: foundAplPin = %q — a phantom pin defers every env forever", in, got)
		}
	}

	// ...and a real key after the block still drops.
	after := "spec:\n  notes: |\n    some text\n  bootstrap:\n    aplChartVersion: v6.2.0\n"
	if _, dropped, refused := dropTrackingPin(after); refused || dropped != "v6.2.0" {
		t.Errorf("a key after the block closes must still drop, got dropped=%q refused=%v", dropped, refused)
	}
}

// THE FLOW PATH MUST INHERIT THE BLOCK PATH'S DISCIPLINE. It was added as a
// shortcut past the refusal and skipped three guarantees the block path already
// made — each of which turned "we could not act" into "we acted, wrongly".
func TestFlowPathFailsClosedLikeTheBlockPath(t *testing.T) {
	t.Run("two flow keys are ambiguous", func(t *testing.T) {
		// The flow branch computed `mentions` and ignored it, so one key was removed
		// and the other left, and the sweep printed the green "no longer pins" line
		// for a file that still pins.
		in := "a: { aplChartVersion: v6.2.0 }\nb: { aplChartVersion: v6.1.0 }\n"
		out, _, refused := dropTrackingPin(in)
		if !refused {
			t.Fatalf("two active keys must be refused whatever shape they are in, got:\n%q", out)
		}
		if out != in {
			t.Error("a refused file must be left byte-identical")
		}
	})

	t.Run("an operator's flow pin is reported, not lost", func(t *testing.T) {
		// sweepAplPins re-derived the kept pin with the line-anchored regex, which
		// cannot see a flow mapping — so this landed in none of Dropped/Kept/Refused.
		in := "      bootstrap: { name: p, aplChartVersion: 6.0.1 }\n"
		if _, dropped, refused := dropTrackingPin(in); dropped != "" || refused {
			t.Fatalf("a deliberate flow pin is neither dropped nor refused, got dropped=%q refused=%v", dropped, refused)
		}
		if got := foundAplPin(in); got != "6.0.1" {
			t.Errorf("foundAplPin = %q, want 6.0.1 — otherwise it is reported as nothing at all", got)
		}
		// And the knock-on: an unparseable flow pin must reach the blocking report,
		// because that spec hard-blocks the very next command.
		if got := foundAplPin("      bootstrap: { aplChartVersion: latest }\n"); got != "latest" {
			t.Errorf("foundAplPin = %q, want latest — the BLOCK warning never fires for a pin nothing finds", got)
		}

		// THROUGH THE SWEEP, not just the helper. foundAplPin being correct is worth
		// nothing if sweepAplPins still re-derives the kept pin with the line-anchored
		// regex — which is exactly the bug, and a test of the helper alone cannot see
		// it. This arm stayed green through a revert of the wiring; it does not now.
		dir := t.TempDir()
		writeInstanceFile(t, dir, filepath.Join("environments", "flow.yaml"), in)
		res, err := sweepAplPins(dir, false)
		if err != nil {
			t.Fatalf("sweepAplPins: %v", err)
		}
		if len(res.Kept) != 1 || res.Kept[0].Pin != "6.0.1" {
			t.Errorf("the sweep must report the flow-style pin as KEPT, got %+v (dropped %+v, refused %+v)",
				res.Kept, res.Dropped, res.Refused)
		}
	})

	t.Run("a commented copy is not the live line", func(t *testing.T) {
		// The edit was applied with strings.Replace — a first-SUBSTRING replace — so a
		// commented-out copy earlier in the file absorbs it: the comment is rewritten,
		// the live pin survives, and the sweep reports a successful drop.
		in := "#      bootstrap: { name: p, aplChartVersion: v6.2.0 }\n      bootstrap: { name: p, aplChartVersion: v6.2.0 }\n"
		out, dropped, refused := dropTrackingPin(in)
		if refused || dropped != "v6.2.0" {
			t.Fatalf("dropped=%q refused=%v", dropped, refused)
		}
		if !strings.HasPrefix(out, "#      bootstrap: { name: p, aplChartVersion: v6.2.0 }\n") {
			t.Errorf("the commented copy must be untouched:\n%q", out)
		}
		if strings.Contains(strings.SplitN(out, "\n", 2)[1], "aplChartVersion") {
			t.Errorf("the LIVE pin must be the one removed:\n%q", out)
		}
	})

	t.Run("a flow line inside a block scalar is prose", func(t *testing.T) {
		// A NO-OP, not a refusal: to the parser this is not a key, so there is
		// nothing to refuse and nothing to nag the operator about.
		in := "spec:\n  notes: |\n    bootstrap: { aplChartVersion: v6.2.0 }\n"
		out, dropped, refused := dropTrackingPin(in)
		if refused || dropped != "" || out != in {
			t.Errorf("prose is not a mapping, got dropped=%q refused=%v out=%q", dropped, refused, out)
		}
	})
}

// AN ENV PIN CANNOT BE RETIRED WHILE spec.defaults STILL PINS. clusterspec
// resolves a deployment as pickStr(defaults, env), so an ABSENT env value falls
// through to the DEFAULT, not to the baseline. Dropping the env pin while the root
// keeps `6.0.1` moves prod from 6.2.0 to 6.0.1 — backward — under a green line
// claiming it now "tracks this release's baseline and every future one": a true
// statement about the file and a false one about the instance.
func TestSweepDefersEnvPinsWhileTheSpecRootStillPins(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 6.0.1\n")
	writeEnv(t, dir, "prod", "v6.2.0")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("nothing may be dropped while the root pins, got %+v", res.Dropped)
	}
	if len(res.Deferred) != 1 || res.Deferred[0].Pin != "v6.2.0" {
		t.Fatalf("prod's pin must be DEFERRED, not dropped, got %+v", res.Deferred)
	}
	if len(res.Kept) != 1 || res.Kept[0].File != "landingzone.yaml" {
		t.Errorf("the root's deliberate pin must be reported as kept, got %+v", res.Kept)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "environments", "prod.yaml"))
	if !strings.Contains(string(b), "aplChartVersion: v6.2.0") {
		t.Errorf("prod must be untouched, or it silently inherits 6.0.1:\n%s", b)
	}

	// The remedy has to name the file that is BLOCKING the retirement, not the one
	// that cannot be retired — the operator has to settle the root first.
	steps := aplPinSteps(nil, nil, nil, []string{"environments/prod.yaml"})
	if len(steps) != 1 || !strings.Contains(steps[0], "landingzone.yaml") {
		t.Errorf("the checklist must point at the spec root, got %q", steps)
	}
}

// ...and with the root NOT pinning, the same env pin retires normally. A guard that
// never releases is a disabled lever.
func TestSweepDropsEnvPinsWhenTheSpecRootIsClean(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        managedAppPlatform: true\n")
	writeEnv(t, dir, "prod", "v6.2.0")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Deferred) != 0 {
		t.Errorf("nothing to defer when the root does not pin, got %+v", res.Deferred)
	}
	if len(res.Dropped) != 1 || res.Dropped[0].Pin != "v6.2.0" {
		t.Fatalf("prod's pin must retire, got %+v", res.Dropped)
	}
}

// A `#` INSIDE A QUOTED SCALAR IS NOT A COMMENT — asserted through the behaviour
// rather than through a helper: the parser knows the difference, and a hand-rolled
// scanner that cut at the first `#` would lose the key entirely.
func TestQuotedHashIsNotAComment(t *testing.T) {
	in := "bootstrap: { name: \"prod#1\", aplChartVersion: v6.2.0 }\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused || dropped != "v6.2.0" {
		t.Fatalf("a `#` inside a quoted scalar is not a comment: dropped=%q refused=%v", dropped, refused)
	}
	if !strings.Contains(out, `name: "prod#1"`) {
		t.Errorf("the operator's scalar must survive intact:\n%q", out)
	}
	if strings.Contains(out, "aplChartVersion") {
		t.Errorf("the pin should be gone:\n%q", out)
	}
}

// A NULL PIN IS NOT A PIN. `aplChartVersion: ~` decodes to "", which is what an
// omitted key gives the loader — drift None, and the spec gate returns nil. Grading
// the raw TEXT filed it as a pin that "will BLOCK validation", with a numbered step,
// for a spec that validates fine.
func TestNullPinIsNotAPin(t *testing.T) {
	for _, in := range []string{
		"cluster:\n  bootstrap:\n    aplChartVersion: ~\n",
		"cluster:\n  bootstrap:\n    aplChartVersion:\n",
	} {
		out, dropped, refused := dropTrackingPin(in)
		if refused || dropped != "" || out != in {
			t.Errorf("%q: a null pin is nothing to act on, got dropped=%q refused=%v", in, dropped, refused)
		}
		if got := foundAplPin(in); got != "" {
			t.Errorf("%q: foundAplPin = %q, want empty — otherwise it is reported as a pin that blocks", in, got)
		}
	}
}

// A REFUSAL IS NOT A DEFERRAL. Both carry a pin in the same return value, so keying
// the deferral short-circuit on `dropped != ""` alone filed an UNREWRITABLE file as
// merely "wait for the root" — and handed it the deferral's remedy, settle the root
// first, when its actual problem is that nothing can rewrite it.
func TestSweepDoesNotFileARefusalAsDeferred(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 6.0.1\n")
	// Two active keys: unrewritable whatever the root does.
	writeInstanceFile(t, dir, filepath.Join("environments", "prod.yaml"),
		"a:\n      aplChartVersion: v6.2.0\nb:\n      aplChartVersion: v6.1.0\n")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Deferred) != 0 {
		t.Errorf("an unrewritable file is refused, not deferred, got %+v", res.Deferred)
	}
	if len(res.Refused) != 1 || res.Refused[0].File != filepath.Join("environments", "prod.yaml") {
		t.Errorf("want prod.yaml refused, got %+v", res.Refused)
	}
}

// THE DRY RUN MUST NOT ANSWER FOR A WRITE IT DID NOT MAKE. rootPin was re-read from
// DISK, so a dry run said landingzone.yaml "would stop pinning" and, in the same
// breath, that prod was left alone "because landingzone.yaml still pins too" — plus
// a next step telling the operator to hand-edit a root the real run retires itself.
func TestDryRunSeesTheRootItWouldHaveWritten(t *testing.T) {
	dir := t.TempDir()
	// Both are ours, so a real run retires both and nothing is deferred.
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: v6.2.0\n")
	writeEnv(t, dir, "prod", "v6.2.0")

	dry, err := sweepAplPins(dir, false)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(dry.Deferred) != 0 {
		t.Errorf("the dry run must not defer against a root pin it would itself remove, got %+v", dry.Deferred)
	}
	if len(dry.Dropped) != 2 {
		t.Errorf("both pins would retire, got %+v", dry.Dropped)
	}

	// And the real run must agree with what the dry run promised.
	wet, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(wet.Dropped) != len(dry.Dropped) || len(wet.Deferred) != len(dry.Deferred) {
		t.Errorf("dry run and real run disagree: dry=%+v/%+v wet=%+v/%+v", dry.Dropped, dry.Deferred, wet.Dropped, wet.Deferred)
	}
}

// UNPARSEABLE IS NOT THE ONLY WAY A KEPT PIN BLOCKS. A pin BELOW THE SUPPORTED
// FLOOR parses perfectly and still hard-fails `llz ci assert-apl-version` — and it
// is not hypothetical: the `llz import init` this branch retired seeded exactly such
// pins. Filing it as "deliberate, left alone" with a Review step described a spec
// that stops the next command as something to get to later.
//
// THE FLOOR, not major drift, and the distinction cost a round to learn: the richer
// drift gate has no production caller, so a major-AHEAD pin blocks nothing and a
// below-floor pin blocks via a check the drift override cannot release.
func TestRetrackAplPinsFlagsABelowFloorPinAsBlocking(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	writeEnv(t, dir, "legacy", "5.0.0")
	t.Chdir(dir)

	var steps []string
	out := captureStderr(t, func() { steps = retrackAplPins(false) })
	joined := strings.Join(steps, "\n")
	if !strings.Contains(out, "BLOCK") {
		t.Errorf("a below-floor pin must be named as blocking, got:\n%s", out)
	}
	if strings.Contains(out, clusterspec.AllowMajorDriftEnv) {
		t.Errorf("the drift override does not release the FLOOR check — naming it sends the operator to a switch that cannot help:\n%s", out)
	}
	if !strings.Contains(joined, "BLOCK") || !strings.Contains(joined, "legacy.yaml") {
		t.Errorf("the checklist must carry it as blocking, got %q", joined)
	}
	if strings.Contains(joined, "it is not one llz set") {
		t.Errorf("a blocking pin must not be filed under the deliberate-choice remedy, got %q", joined)
	}
}

// THE `{ }` TIDY MUST BE SCOPED TO THE MAPPING IT EMPTIED. It was a whole-line
// ReplaceAll, so an unrelated `extra: { }` on the same line was reformatted too —
// the gratuitous-diff class the spacing handling elsewhere exists to avoid.
func TestFlowTidyDoesNotTouchNeighbours(t *testing.T) {
	in := "a: { aplChartVersion: v6.2.0 }\nb: { }\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused || dropped != "v6.2.0" {
		t.Fatalf("dropped=%q refused=%v", dropped, refused)
	}
	if !strings.Contains(out, "b: { }") {
		t.Errorf("the neighbouring mapping must keep its own formatting:\n%q", out)
	}
	// AND THE EMPTIED MAPPING COLLAPSES CLEANLY. This test used exactly this input
	// and asserted only the neighbour, so it passed while the edited mapping came out
	// as `{  }` — a test green on the wrong output, which is worse than no test.
	if !strings.HasPrefix(out, "a: {}\n") {
		t.Errorf("the emptied mapping must collapse to `{}`, got:\n%q", out)
	}
}

// Every spacing form an operator might have written collapses the same way, and a
// mapping that still has members is left exactly as it was.
func TestFlowTidyCollapsesWhateverTheSpacing(t *testing.T) {
	for in, want := range map[string]string{
		"a: { aplChartVersion: v6.2.0 }\n":       "a: {}\n",
		"a: {aplChartVersion: v6.2.0}\n":         "a: {}\n",
		"a: {  aplChartVersion: v6.2.0  }\n":     "a: {}\n",
		"a: { aplChartVersion: v6.2.0, b: 1 }\n": "a: { b: 1 }\n",
		"a: { b: 1, aplChartVersion: v6.2.0 }\n": "a: { b: 1 }\n",
	} {
		if out, _, refused := dropTrackingPin(in); refused || out != want {
			t.Errorf("dropTrackingPin(%q)\n got %q\nwant %q (refused=%v)", in, out, want, refused)
		}
	}
}

// The refusal names the cause, and the three causes are distinguishable — an
// operator handed the wrong one goes looking for a problem their file does not have.
func TestRefusalReasonNamesTheActualCause(t *testing.T) {
	cases := map[string]string{
		"a:\n  aplChartVersion: v6.2.0\nb:\n  aplChartVersion: v6.1.0\n": "more than one aplChartVersion key",
		"a: [\n  # aplChartVersion: v6.2.0\n":                            "does not parse as YAML",
		"a:\n  aplChartVersion: |\n    v6.2.0\n":                         "not a plain scalar on the key's own line",
	}
	for in, want := range cases {
		if got := refusalReason(in); !strings.Contains(got, want) {
			t.Errorf("refusalReason(%q) = %q, want it to contain %q", in, got, want)
		}
	}
}

// THE PARSER IS THE SINGLE SOURCE OF TRUTH ABOUT WHAT A PIN IS, and this is the
// property twelve rounds of hand-rolled scanning kept failing in a new way: comments,
// quoted `#`, block scalars, flow mappings and duplicate keys are all its job now.
// Each row is a shape a hand-rolled scanner reads wrongly.
func TestPinDetectionAcrossYAMLShapes(t *testing.T) {
	cases := []struct {
		name, in string
		want     string // the pin the sweep should see, "" for none
	}{
		{"block style", "a:\n  aplChartVersion: v6.2.0\n", "v6.2.0"},
		{"quoted value", "a:\n  aplChartVersion: \"v6.2.0\"\n", "v6.2.0"},
		{"trailing comment", "a:\n  aplChartVersion: v6.2.0   # note, with a comma\n", "v6.2.0"},
		{"commented out", "a:\n  # aplChartVersion: v6.2.0\n  b: 1\n", ""},
		{"hash in a quoted scalar", "a: { name: \"p#1\", aplChartVersion: v6.2.0 }\n", "v6.2.0"},
		{"block scalar body", "a:\n  notes: |\n    aplChartVersion: v6.2.0\n", ""},
		{"single-line flow", "a: { aplChartVersion: v6.2.0 }\n", "v6.2.0"},
		{"multi-line flow", "a: {\n  aplChartVersion: v6.2.0\n}\n", "v6.2.0"},
		{"null", "a:\n  aplChartVersion: ~\n", ""},
		{"nested deeply", "spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: v6.2.0\n", "v6.2.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := foundAplPin(tc.in); got != tc.want {
				t.Errorf("foundAplPin = %q, want %q", got, tc.want)
			}
		})
	}
}

// yaml.Node.Column COUNTS CHARACTERS, NOT BYTES. Adding it straight to a byte
// offset was silent corruption: the splice landed mid-sequence and produced a file
// that no longer parses, while the sweep reported a clean drop. Only the flow path
// was affected — the block path's under-count cannot cross a newline — which is
// exactly why every other test stayed green.
func TestFlowSpliceIsByteAccurateWithNonASCII(t *testing.T) {
	cases := map[string]string{
		`bootstrap: { name: "日本語テスト", aplChartVersion: v6.2.0, managedAppPlatform: true }` + "\n": `bootstrap: { name: "日本語テスト", managedAppPlatform: true }` + "\n",
		`bootstrap: { name: "café", aplChartVersion: v6.2.0 }` + "\n":                             `bootstrap: { name: "café" }` + "\n",
		`bootstrap: { emoji: "🚀", aplChartVersion: v6.2.0, b: 1 }` + "\n":                         `bootstrap: { emoji: "🚀", b: 1 }` + "\n",
	}
	for in, want := range cases {
		out, dropped, refused := dropTrackingPin(in)
		if refused || dropped != "v6.2.0" {
			t.Errorf("%q: dropped=%q refused=%v", in, dropped, refused)
			continue
		}
		if out != want {
			t.Errorf("non-ASCII splice\n got %q\nwant %q", out, want)
		}
		// The strongest arm: whatever we produced must still be YAML. A corrupted
		// splice reported success, so "did it change" was never the question.
		var probe map[string]any
		if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
			t.Errorf("the sweep produced a file that does not parse: %v\n%q", err, out)
		}
	}
}

// AN UNREADABLE ROOT IS NOT AN UNPINNED ROOT. foundAplPin swallows the parse error
// and answers "", so a malformed landingzone.yaml read as "no default pin" and every
// env pin was deleted underneath it — and once the operator fixes the syntax error
// those deployments resolve to the default that was there all along. The same
// backward move rootFirst/Deferred was added to prevent, arriving through the
// refusal path instead of the kept one.
func TestUnreadableSpecRootDefersEnvPins(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml", "spec: [\n  defaults: aplChartVersion: 6.0.1\n")
	writeEnv(t, dir, "prod", "v6.2.0")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("nothing may be dropped while the root cannot be read, got %+v", res.Dropped)
	}
	if len(res.Deferred) != 1 || res.Deferred[0].Pin != "v6.2.0" {
		t.Fatalf("prod's pin must be DEFERRED, got %+v", res.Deferred)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "environments", "prod.yaml"))
	if !strings.Contains(string(b), "aplChartVersion: v6.2.0") {
		t.Errorf("prod must be untouched:\n%s", b)
	}
}

// A PIN THAT OPENS A SEQUENCE ENTRY MUST BE REFUSED. removeBlockLine deletes the
// whole physical line, so `- aplChartVersion: v6.2.0` would take the `- ` with it
// and silently convert that sequence element into a sibling mapping key. The output
// still PARSES, which is what makes it dangerous — every other shape the splice
// cannot handle is refused, and this one changed the document's meaning while
// looking like a clean edit.
func TestDropTrackingPinRefusesASequenceEntry(t *testing.T) {
	in := "overrides:\n  - aplChartVersion: v6.2.0\n    name: p\n"
	out, _, refused := dropTrackingPin(in)
	if !refused {
		t.Fatalf("a pin opening a sequence entry must be refused, got:\n%q", out)
	}
	if out != in {
		t.Error("a refused file must be left byte-identical")
	}

	// A pin that merely sits INSIDE a sequence entry, on its own line, is an
	// ordinary mapping key and still retires — the rule is about what shares the
	// line, not about sequences.
	ok := "overrides:\n  - name: p\n    aplChartVersion: v6.2.0\n"
	if _, dropped, refused := dropTrackingPin(ok); refused || dropped != "v6.2.0" {
		t.Errorf("a pin on its own line inside a sequence entry must still drop, got dropped=%q refused=%v", dropped, refused)
	}
}

// AN UNPARSEABLE ROOT AND A ROOT THAT STILL PINS ARE DIFFERENT PROBLEMS with
// different remedies — one is fixed by removing a pin, the other by fixing syntax.
// An unparseable root that never mentions aplChartVersion produced no Kept or
// Refused entry of its own, so the operator was told "landingzone.yaml still pins
// too" and handed a step to remove a pin that does not exist, while the actual fault
// went unnamed.
func TestDeferralNamesWhyTheRootBlockedIt(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml", "spec: [\n  defaults: oops\n")
	writeEnv(t, dir, "prod", "v6.2.0")

	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Deferred) != 1 {
		t.Fatalf("prod must be deferred behind an unreadable root, got %+v", res.Deferred)
	}
	if !strings.Contains(res.Deferred[0].Reason, "does not parse") {
		t.Errorf("the deferral must name the real fault, got %q", res.Deferred[0].Reason)
	}
	if strings.Contains(res.Deferred[0].Reason, "still pins") {
		t.Errorf("it must not claim a pin that does not exist, got %q", res.Deferred[0].Reason)
	}

	// And the other cause still reads as itself.
	dir2 := t.TempDir()
	writeInstanceFile(t, dir2, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 6.0.1\n")
	writeEnv(t, dir2, "prod", "v6.2.0")
	res2, err := sweepAplPins(dir2, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res2.Deferred) != 1 || !strings.Contains(res2.Deferred[0].Reason, "still pins 6.0.1") {
		t.Errorf("a root that pins must say so, and name it, got %+v", res2.Deferred)
	}
}

// A MAJOR-AHEAD PIN BLOCKS NOTHING TODAY, so it must not be announced as blocking.
// The gate that would stop it (clusterspec.aplChartVersionError) has no production
// caller; the delivered preflight is a floor check, and 7.0.0 clears the floor.
func TestRetrackAplPinsDoesNotCallAMajorAheadPinBlocking(t *testing.T) {
	dir := newRenderableInstance(t, "v0.4.0")
	writeEnv(t, dir, "ahead", "7.0.0")
	t.Chdir(dir)

	var steps []string
	out := captureStderr(t, func() { steps = retrackAplPins(false) })
	joined := strings.Join(steps, "\n")
	if strings.Contains(out, "BLOCK") || strings.Contains(joined, "BLOCK") {
		t.Errorf("7.0.0 clears the supported floor, so nothing blocks on it:\n%s\n%s", out, joined)
	}
	if !strings.Contains(joined, "ahead.yaml") {
		t.Errorf("it is still a pin llz did not set and deserves a review line, got %q", joined)
	}
}

// CRLF ON THE FLOW PATH. The backward separator scan skipped space, tab and \n but
// not \r, so a CRLF spec whose flow mapping ends on the pin kept its preceding
// comma. It still parses, so nothing caught it — the CRLF test covered only the
// block path.
func TestFlowSpliceHandlesCRLF(t *testing.T) {
	in := "bootstrap: {\r\n    name: p,\r\n    aplChartVersion: v6.2.0\r\n  }\r\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused || dropped != "v6.2.0" {
		t.Fatalf("dropped=%q refused=%v", dropped, refused)
	}
	if strings.Contains(out, ",") {
		t.Errorf("the separator must go with the entry it separated:\n%q", out)
	}
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
		t.Errorf("the result must still parse: %v\n%q", err, out)
	}
}

// AN ANCHORED PIN IS NEVER A LOCAL EDIT. `aplChartVersion: &aplver v6.2.0` is a
// plain scalar on the key's own line, so every other guard passed it — and deleting
// the line took the ANCHOR DEFINITION with it, leaving `*aplver` dangling and the
// spec unparseable, under a green "no longer pins" line.
func TestDropTrackingPinRefusesAnchors(t *testing.T) {
	cases := []struct{ name, in string }{
		{"anchor on the value", "spec:\n  bootstrap:\n    aplChartVersion: &aplver v6.2.0\n  other:\n    version: *aplver\n"},
		{"anchor on the enclosing mapping", "base: &base\n  aplChartVersion: v6.2.0\nprod:\n  <<: *base\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, refused := dropTrackingPin(tc.in)
			if !refused {
				t.Fatalf("must be refused, got:\n%q", out)
			}
			if out != tc.in {
				t.Error("a refused file must be left byte-identical")
			}
			// The arm that matters: whatever we leave behind must still parse. The
			// bug's whole signature was a broken file reported as a clean drop.
			var probe map[string]any
			if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
				t.Errorf("the file must still parse: %v", err)
			}
		})
	}

	// And the refusal says WHY, since "anchor" is not something the operator will
	// guess from a file that looks ordinary.
	if got := refusalReason("a:\n  aplChartVersion: &v v6.2.0\n"); !strings.Contains(got, "anchor") {
		t.Errorf("refusalReason = %q, want it to name the anchor", got)
	}
}

// ...but an env with NO pin of its own inherits the default, so the same
// below-floor root DOES block. A guard that never fires is a disabled guard.
func TestBelowFloorDefaultBlocksWhenAnEnvInheritsIt(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 5.0.0\n")
	writeEnv(t, dir, "prod", "") // no pin: resolves through spec.defaults
	t.Chdir(dir)

	var steps []string
	out := captureStderr(t, func() { steps = retrackAplPins(false) })
	if !strings.Contains(out, "BLOCK") || !strings.Contains(strings.Join(steps, "\n"), "BLOCK") {
		t.Errorf("an inherited below-floor default must be named as blocking:\n%s\n%v", out, steps)
	}
}

// THE FIRST SITE IN DOCUMENT ORDER IS NOT NECESSARILY THE PINNING ONE. Returning
// sites[0] made a root whose first aplChartVersion is null read as UNPINNED, so the
// deferral was skipped and every env pin was deleted underneath a default that
// really pins — resolving those deployments backward the moment the file is loaded.
func TestFoundAplPinSkipsNullSites(t *testing.T) {
	root := "spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion:\n  other:\n    aplChartVersion: 6.0.1\n"
	if got := foundAplPin(root); got != "6.0.1" {
		t.Errorf("foundAplPin = %q, want 6.0.1 — a null first site must not hide a real pin", got)
	}

	// End to end: prod must be DEFERRED behind that root, not dropped.
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml", root)
	writeEnv(t, dir, "prod", "v6.2.0")
	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("nothing may be dropped while the root still pins, got %+v", res.Dropped)
	}
	if len(res.Deferred) != 1 {
		t.Fatalf("prod must be deferred, got %+v", res.Deferred)
	}
}

// CRLF ON THE LAST ENTRY OF A MULTI-LINE FLOW MAPPING. entryEnd stops at the \n, so
// the \r sits just before it and survives the trim, leaving a bare LF in an otherwise
// CRLF file.
func TestFlowSpliceKeepsCRLFConsistent(t *testing.T) {
	in := "bootstrap: {\r\n    name: p,\r\n    aplChartVersion: v6.2.0\r\n  }\r\n"
	out, dropped, refused := dropTrackingPin(in)
	if refused || dropped != "v6.2.0" {
		t.Fatalf("dropped=%q refused=%v", dropped, refused)
	}
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Errorf("every line ending must stay CRLF, got:\n%q", out)
	}
}

// A ROOT THAT IS BLOCKING SOMETHING IS NOT HARMLESS. When env pins are DEFERRED
// because of the root, those envs keep their own pins — so RootInherited stays false
// and the root looked overridden. The report then said "every deployment overrides
// it, nothing resolves to it" and, two lines later, that those same envs "cannot be
// retired until landingzone.yaml is settled". Short-circuiting there also skipped the
// blocking arms, so a below-floor root was filed as an optional Review — and the
// moment the operator followed the deferral advice, that deployment resolved to it
// and assert-apl-version hard-failed.
func TestARootThatDefersEnvsIsNotReportedHarmless(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 5.0.0\n")
	writeEnv(t, dir, "prod", "v6.2.0") // one of ours: slated to retire, hence deferred
	t.Chdir(dir)

	var steps []string
	out := captureStderr(t, func() { steps = retrackAplPins(false) })
	joined := strings.Join(steps, "\n")
	if strings.Contains(out, "every deployment overrides it") {
		t.Errorf("prod is deferred BECAUSE of this pin — it is not overridden:\n%s", out)
	}
	if !strings.Contains(out, "BLOCK") || !strings.Contains(joined, "BLOCK") {
		t.Errorf("a below-floor root that envs are waiting on must be named as blocking:\n%s\n%s", out, joined)
	}
}

// AN ALIAS VALUE IS NOT A SCALAR PIN. `aplChartVersion: *ver` keeps its real value
// somewhere else, and Node.Value carries the alias NAME — so it was graded as the
// pin "ver": a numbered must-fix step claiming a valid spec "will BLOCK
// assert-apl-version", and every env deferred behind a root that "still pins ver".
func TestDropTrackingPinRefusesAliasValues(t *testing.T) {
	in := "vars:\n  ver: &ver v6.2.1\ncluster:\n  bootstrap:\n    aplChartVersion: *ver\n"
	out, _, refused := dropTrackingPin(in)
	if !refused {
		t.Fatalf("an alias value must be refused, got:\n%q", out)
	}
	if out != in {
		t.Error("a refused file must be left byte-identical")
	}
	if got := foundAplPin(in); got == "ver" {
		t.Errorf("foundAplPin must not report the alias NAME as a version, got %q", got)
	}
	if got := refusalReason(in); !strings.Contains(got, "alias") {
		t.Errorf("the refusal must name the alias, got %q", got)
	}
}

// "EVERY DEPLOYMENT OVERRIDES IT" IS VACUOUS WITH NO DEPLOYMENTS. With no
// environments/*.yaml, no env inherits the default — trivially — and the pin was
// filed as an optional Review while the short-circuit skipped both blocking arms.
// The next `llz env add` inherits it and assert-apl-version hard-fails on a spec this
// command called fine.
func TestARootPinIsNotHarmlessWhenThereAreNoEnvs(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 5.0.0\n")
	t.Chdir(dir)

	var steps []string
	out := captureStderr(t, func() { steps = retrackAplPins(false) })
	if strings.Contains(out, "every deployment overrides it") {
		t.Errorf("there are no deployments to override it:\n%s", out)
	}
	if !strings.Contains(out, "BLOCK") || !strings.Contains(strings.Join(steps, "\n"), "BLOCK") {
		t.Errorf("a below-floor default with nothing overriding it must be named as blocking:\n%s\n%v", out, steps)
	}
}

// AN OVERRIDDEN DEFAULT IS NOT A HARMLESS ONE. Every CURRENT environment overriding
// it is not the end of it: the next `llz env add` writes no pin, inherits the
// default, and assert-apl-version hard-fails on a spec this command called fine. The
// override is worth SAYING, not worth suppressing the verdict for.
func TestAnOverriddenRootPinIsStillGraded(t *testing.T) {
	for _, pin := range []string{"5.0.0", "latest"} {
		t.Run(pin, func(t *testing.T) {
			dir := t.TempDir()
			writeInstanceFile(t, dir, "landingzone.yaml",
				"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: "+pin+"\n")
			writeEnv(t, dir, "prod", "6.0.1") // never a baseline: KEPT, a permanent override
			t.Chdir(dir)

			var steps []string
			out := captureStderr(t, func() { steps = retrackAplPins(false) })
			joined := strings.Join(steps, "\n")
			if !strings.Contains(out, "BLOCK") || !strings.Contains(joined, "BLOCK") {
				t.Errorf("a new deployment would inherit %q, so it must still be named as blocking:\n%s\n%s", pin, out, joined)
			}
			if !strings.Contains(out, "currently overrides it") {
				t.Errorf("the override is context worth giving, got:\n%s", out)
			}
		})
	}

	// A root pin that is FINE stays a plain note, not a blocking one.
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 6.0.1\n")
	writeEnv(t, dir, "prod", "6.0.2")
	t.Chdir(dir)
	out := captureStderr(t, func() { retrackAplPins(false) })
	if strings.Contains(out, "BLOCK") {
		t.Errorf("a supported default blocks nothing:\n%s", out)
	}
}

// A COMMENT ATTACHED ABOVE THE PIN IS REFUSED, NOT REWRITTEN.
//
// The hazard is real — an orphaned `# renovate:` rebinds to the next key — but the
// comment cannot be taken along safely: nothing here distinguishes a section header
// from a note about this pin, and yaml.v3 attaches a document's leading comments to
// its first key, so a top-level pin would carry the file header away.
func TestCommentAttachedPinsAreRefused(t *testing.T) {
	cases := map[string]string{
		"prose above":                        "a:\n  bootstrap:\n    # held until the 6.3 upgrade\n    aplChartVersion: v6.2.0\n    name: p\n",
		"annotation above":                   "a:\n  bootstrap:\n    # renovate: datasource=helm depName=apl\n    aplChartVersion: v6.2.0\n",
		"commented example above":            "a:\n  bootstrap:\n      # aplChartVersion: v6.1.0\n      aplChartVersion: v6.2.0\n",
		"flow entry with a trailing comment": "bootstrap: { aplChartVersion: v6.2.0, # renovate: x\n  name: p }\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, _, refused := dropTrackingPin(in)
			if !refused {
				t.Fatalf("must be refused rather than guessed at, got:\n%q", out)
			}
			if out != in {
				t.Errorf("a refused file must be left byte-identical:\n%q", out)
			}
			if got := refusalReason(in); !strings.Contains(got, "comment") {
				t.Errorf("the refusal must name the comment, got %q", got)
			}
		})
	}
}

// AND THE ORDINARY CASE STILL RETIRES. Refusing what cannot be read safely is only
// defensible while the lever keeps doing its job on the common shapes — including a
// trailing comment on the pin's OWN line, which goes with the line and needs no
// judgement about whose comment it is.
func TestPlainPinsStillRetire(t *testing.T) {
	cases := map[string]string{
		"block":                        "a:\n  bootstrap:\n    aplChartVersion: v6.2.0\n    name: p\n",
		"block with trailing comment":  "a:\n  bootstrap:\n    aplChartVersion: v6.2.0  # pinned during the rollout, revisit at 6.3\n    name: p\n",
		"flow, no comments":            "bootstrap: { aplChartVersion: v6.2.0, name: p }\n",
		"flow with a quoted separator": `bootstrap: { name: "a,b}", aplChartVersion: v6.2.0 }` + "\n",
		"multi-line flow":              "bootstrap: {\n  name: p,\n  aplChartVersion: v6.2.0\n}\n",
		// A blank line makes these the DOCUMENT's comments rather than the key's, so
		// the pin carries nothing away and the header stays — which is why the
		// refusal set does not need this shape.
		"file header, blank line, leading pin": "# LandingZone spec for prod\n# maintained by the platform team\n\naplChartVersion: v6.2.0\nname: p\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, dropped, refused := dropTrackingPin(in)
			if refused || dropped != "v6.2.0" {
				t.Fatalf("dropped=%q refused=%v", dropped, refused)
			}
			if strings.Contains(out, "aplChartVersion") {
				t.Errorf("the pin should be gone:\n%q", out)
			}
			var probe map[string]any
			if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
				t.Errorf("the result must still parse: %v\n%q", err, out)
			}
			// Nothing that was not the pin may disappear with it.
			// The pin's OWN trailing comment is part of its line and goes with it; what
			// must survive is everything that was never the pin's.
			for _, keep := range []string{"name: p", "LandingZone spec for prod", "maintained by the platform team"} {
				if strings.Contains(in, keep) && !strings.Contains(out, keep) {
					t.Errorf("%q vanished with the pin:\n%q", keep, out)
				}
			}
		})
	}
}

// A COMMENT MUST NOT SUPPRESS THE DIAGNOSTICS. The head-comment refusal sat ABOVE the
// ownership check, so a pin llz would never touch landed in Refused instead of Kept —
// and Kept is the only path that runs the blocking arms. `# renovate:` above
// `aplChartVersion: latest` silently lost its "will BLOCK assert-apl-version"
// warning, which the very same spec reports correctly without the comment.
//
// The two refusals sit on opposite sides of that check for a reason: an anchor means
// the VALUE is unknowable and must be refused first; a comment means only that the
// line is not safely EDITABLE, and the value is perfectly legible.
func TestACommentDoesNotSuppressTheBlockingWarning(t *testing.T) {
	for _, pin := range []string{"latest", "5.0.0"} {
		t.Run(pin, func(t *testing.T) {
			dir := newRenderableInstance(t, "v0.4.0")
			writeInstanceFile(t, dir, filepath.Join("environments", "prod.yaml"),
				"cluster:\n  bootstrap:\n    # renovate: datasource=helm depName=apl\n    aplChartVersion: "+pin+"\n")
			t.Chdir(dir)

			var steps []string
			out := captureStderr(t, func() { steps = retrackAplPins(false) })
			if !strings.Contains(out, "BLOCK") || !strings.Contains(strings.Join(steps, "\n"), "BLOCK") {
				t.Errorf("a comment must not hide that %q blocks:\n%s\n%v", pin, out, steps)
			}
		})
	}
}

// EVERY DOCUMENT, NOT JUST THE FIRST. yaml.Unmarshal decodes one document and stops
// WITHOUT error, so a multi-document spec whose pin lives in the second read as
// unpinned and well-formed — rootPin "", rootUnreadable false — and every env pin was
// deleted under a root that still pins. The fourth distinct route into the same
// backward resolution, and one no existing guard covered, because they all trust
// findPinSites to have looked.
func TestFindPinSitesReadsEveryDocument(t *testing.T) {
	multi := "---\nsomething: else\n---\nspec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 6.0.1\n"
	if got := foundAplPin(multi); got != "6.0.1" {
		t.Errorf("foundAplPin = %q, want 6.0.1 — a pin in the second document is still a pin", got)
	}

	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml", multi)
	writeEnv(t, dir, "prod", "v6.2.0")
	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if len(res.Dropped) != 0 {
		t.Errorf("nothing may be dropped under a root that still pins, got %+v", res.Dropped)
	}
	if len(res.Deferred) != 1 {
		t.Errorf("prod must be deferred, got %+v", res.Deferred)
	}
}

// DEFERRED ENVS ARE STILL ENVS. The increment sat below the deferral's `continue`, so
// they went uncounted — masked only because the sole consumer also requires
// len(Deferred) == 0. A guard correct by coincidence is one edit from being wrong.
func TestDeferredEnvsAreCounted(t *testing.T) {
	dir := t.TempDir()
	writeInstanceFile(t, dir, "landingzone.yaml",
		"spec:\n  defaults:\n    cluster:\n      bootstrap:\n        aplChartVersion: 6.0.1\n")
	writeEnv(t, dir, "prod", "v6.2.0") // ours, so deferred behind the root
	res, err := sweepAplPins(dir, true)
	if err != nil {
		t.Fatalf("sweepAplPins: %v", err)
	}
	if res.Envs != 1 {
		t.Errorf("Envs = %d, want 1 — a deferred environment is still an environment", res.Envs)
	}
}

// THE REFUSAL REASON MUST MATCH WHAT ACTUALLY REFUSED. refusalReason ordered its
// cases differently from dropTrackingPin, so a pin that is BOTH anchored and
// comment-attached was stopped by the anchor and explained by the comment — handing
// the operator a remedy that does not address what happened. Two orderings of one
// rule is a second copy, and it drifted the moment either gained an arm.
func TestRefusalReasonMatchesTheActualRefusal(t *testing.T) {
	both := "a:\n  bootstrap:\n    # a note\n    aplChartVersion: &ver v6.2.0\n"
	if _, _, refused := dropTrackingPin(both); !refused {
		t.Fatal("anchored and commented must refuse")
	}
	got := refusalReason(both)
	if !strings.Contains(got, "anchor") {
		t.Errorf("the anchor is what stopped it, so that is what must be explained, got %q", got)
	}
	if strings.Contains(got, "comment is attached") {
		t.Errorf("naming the comment sends the operator to a remedy that would not have helped, got %q", got)
	}
	// Comment alone still reads as the comment.
	only := "a:\n  bootstrap:\n    # a note\n    aplChartVersion: v6.2.0\n"
	if got := refusalReason(only); !strings.Contains(got, "comment") {
		t.Errorf("a comment-only refusal must name the comment, got %q", got)
	}
}

// EVERY REFUSAL NAMES SOMETHING THE OPERATOR CAN ACT ON. Two arms had none, so a
// block-sequence pin was explained as "cannot safely rewrite (its shape is not one
// this upgrade can rewrite)" — a parenthetical restating the sentence around it.
func TestEveryRefusalReasonIsActionable(t *testing.T) {
	cases := map[string]string{
		"sequence entry": "overrides:\n  - aplChartVersion: v6.2.0\n    name: p\n",
		"duplicate keys": "a:\n  aplChartVersion: v6.2.0\nb:\n  aplChartVersion: v6.1.0\n",
		"anchor":         "a:\n  aplChartVersion: &ver v6.2.0\n",
		"head comment":   "a:\n  # a note\n  aplChartVersion: v6.2.0\n",
		"block scalar":   "a:\n  aplChartVersion: |\n    v6.2.0\n",
		"does not parse": "a: [\n  aplChartVersion: v6.2.0\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, refused := dropTrackingPin(in); !refused {
				t.Fatalf("expected a refusal for %q", in)
			}
			got := refusalReason(in)
			if strings.Contains(got, "its shape is not one this upgrade can rewrite") {
				t.Errorf("the catch-all names nothing actionable; this arm needs its own reason, got %q", got)
			}
			if len(got) < 20 {
				t.Errorf("reason too thin to act on: %q", got)
			}
		})
	}
}
