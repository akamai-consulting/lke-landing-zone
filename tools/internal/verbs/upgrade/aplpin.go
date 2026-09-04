package upgrade

// aplpin.go — retires the apl-core pin an upgrade has just made stale.
//
// `spec.cluster.bootstrap.aplChartVersion` is optional, and an omitted pin resolves
// to clusterspec.BaselineAplChartVersion. So an environment that writes the baseline
// into the field states what the default already says — correct the day it is
// written, wrong the day the baseline moves, and `llz validate` then warns about it
// forever until someone edits the file by hand. Same shape as repin.go's `?ref=`: a
// value the TEMPLATE controls, in a file the operator owns, stale on every upgrade
// with no operator judgement involved.
//
// It REMOVES rather than rewrites. Writing the new baseline in would need doing
// again on every future bump; removing it makes the environment track the baseline
// by construction.
//
// Only a version llz itself has targeted is removed (clusterspec.WasAplBaseline).
// Anything else is the operator's and survives. The one case that cannot be told
// apart is a deliberate hold at a version that HAPPENS to be a past baseline: it
// reads identically to a pin left behind, and it is dropped. That trade is bounded —
// every drop is printed per file and lands as a diff in a reviewable pull request,
// and on managed App Platform the pin normally reaches no cluster (Linode owns the
// deployed version), so a hold holds only what `llz ci assert-apl-version` resolves.
//
// UNLESS spec.cluster.bootstrap.manageAplVersion IS SET, which inverts that whole
// premise: the pin then IS what deploys. `llz render` writes it into the apl-overlay,
// the reconciler merges it onto the apl-<env> branch, and apl-core runs its
// runtime-upgrade migrations to get there. Dropping the pin would move a LIVE
// platform to this release's baseline, and the upgrade PR would show only a removed
// line — no version anywhere in the diff to notice. So the sweep refuses outright
// when any deployment owns its version; see manageAplVersionSet.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"unicode/utf8"

	yaml "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

// aplPinKeyName is the spec field this whole file is about.
const aplPinKeyName = "aplChartVersion"

// AplPinChange is one environment file's outcome.
type AplPinChange struct {
	File string
	Pin  string
	// Reason is why a file was Refused, or why a Deferred pin was held back, in the
	// operator's words. Empty for Dropped and Kept — a Dropped entry briefly carried
	// a note about a comment left behind, and that note went when the sweep stopped
	// touching comments at all.
	Reason string
}

// AplPinResult is what the sweep did, in the three outcomes that read differently
// to an operator: pins removed, pins deliberately kept, and files it REFUSED to
// touch. The third is not an empty case of the first — see dropTrackingPin.
type AplPinResult struct {
	Dropped []AplPinChange
	Kept    []AplPinChange
	Refused []AplPinChange
	// Deferred is an env pin that IS one of ours but was left in place anyway,
	// because spec.defaults still carries a pin of its own. See sweepAplPins.
	Deferred []AplPinChange
	// RootInherited is true when at least one environment ends the sweep with no pin
	// of its own, so spec.defaults is what that deployment actually resolves to.
	RootInherited bool
	// Envs counts the environment specs the sweep saw. Zero means "no deployment
	// resolves anything yet", which is NOT the same as "every deployment overrides
	// the default" — see reportAplPinsLeftAlone.
	Envs int
}

// dropTrackingPin removes the aplChartVersion the file declares, when it names a
// version llz has targeted.
//
// FAILS CLOSED ON AMBIGUITY. More than one key means the file is not the shape this
// rule was written against, and editing the wrong one of two pins is worse than
// editing neither. It reports rather than guesses. A file that does not parse is
// refused for the same reason if it mentions the key at all: an unreadable spec is
// not an absent pin.
//
// Returns the new content, the pin removed ("" when nothing was), and whether the
// file was refused.
func dropTrackingPin(content string) (out, dropped string, refused bool) {
	sites, err := findPinSites(content)
	if err != nil {
		return content, "", strings.Contains(content, aplPinKeyName)
	}
	if len(sites) == 0 {
		return content, "", false
	}
	if len(sites) > 1 {
		return content, sites[0].value, true
	}
	s := sites[0]
	// `aplChartVersion: ~` decodes to "", which is what an omitted key gives the
	// loader: no pin, nothing to act on. Grading the raw text instead would call it a
	// pin that blocks validation, for a spec that validates fine.
	if s.value == "" {
		return content, "", false
	}
	if s.keyOff < 0 || s.valOff < 0 {
		return content, s.value, true
	}

	// TWO KINDS OF REFUSAL, ON OPPOSITE SIDES OF THE OWNERSHIP CHECK.
	//
	// An anchor or alias means the VALUE is not knowable from this line — Node.Value
	// carries the alias NAME, so `*ver` would grade as the pin "ver" — so it must be
	// refused before anything is decided about it.
	if s.anchored {
		return content, s.value, true
	}
	if !clusterspec.WasAplBaseline(s.value) {
		return content, "", false // theirs; sweepAplPins finds it via foundAplPin
	}
	// A comment means only that the line is not safely EDITABLE; the value is legible,
	// and everything downstream of knowing it must still happen. Refusing above the
	// ownership check would put a pin llz never set into Refused instead of Kept, and
	// Kept is the only path that runs the blocking diagnostics.
	//
	// The hazard is an orphaned `# renovate:` rebinding to the next key, so renovate
	// proposes bumping the cluster NAME to a chart version. Taking the comment along
	// is not an option: nothing here separates a section header from a note about
	// this pin, and yaml.v3 attaches a document's leading comments to its first key,
	// so a top-level pin would carry the file header away.
	if strings.TrimSpace(s.headComment) != "" {
		return content, s.value, true
	}
	// A flow entry sharing its line with a comment is the shape that corrupts:
	// splicing one entry out leaves a comma behind on one side, or runs the scan past
	// the closing brace on the other. A block pin has no such problem — the whole
	// line goes, its trailing comment with it.
	if s.flow && s.lineComment {
		return content, s.value, true
	}
	if s.flow {
		return removeFlowEntry(content, s), s.value, false
	}
	// A value that is not a plain scalar on the key's own line (a block scalar, an
	// anchor spanning lines) is a shape this splice does not understand.
	if !s.sameLine {
		return content, s.value, true
	}
	// The whole line goes, so nothing may precede the key on it. A pin opening a
	// block sequence entry — `- aplChartVersion: v6.2.0` — would take the `- ` with
	// it and turn that element into a sibling mapping key. The output still parses,
	// which is what makes it worth refusing.
	if strings.TrimSpace(lineAt(content, s.keyOff)[:s.keyOff-lineStart(content, s.keyOff)]) != "" {
		return content, s.value, true
	}
	return removeBlockLine(content, s), s.value, false
}

// lineStart returns the offset at which the line containing off begins.
func lineStart(content string, off int) int {
	if off > len(content) {
		off = len(content)
	}
	return strings.LastIndexByte(content[:off], '\n') + 1
}

// envSpecFiles lists every ACTIVE spec file a pin can live in.
//
// BOTH ALTITUDES, and the second one is easy to miss: `landingzone.yaml` carries
// `spec.defaults.cluster.bootstrap`, which clusterspec's inheritance (mergeCluster in merge.go) folds into every
// environment (clusterspec/merge.go — the env value wins, the default applies when
// it is unset). A pin written there is inherited by every deployment, so sweeping
// only `environments/*.yaml` would leave the single most economical place to put
// one warning forever, in a file this lever never opened.
//
// The delivered `*.yaml.example` is template-managed reference material, not a live
// spec, and carrying a pin is the whole POINT of the example — never edit it. That
// is why the glob is filtered rather than trusted: `landingzone.yaml.example` and
// `environments/*.yaml.example` both sit beside the real files.
func envSpecFiles(root string) ([]string, error) {
	// GLOB WIDE, THEN EXCLUDE, and the width is the point. Globbing `*.yaml`
	// directly cannot match `*.yaml.example` at all — so the exclusion below was
	// unreachable, and the two tests asserting the delivered examples survive an
	// upgrade passed having examined nothing. They would have passed with the
	// protection deleted, which is the vacuous-gate shape this repo refuses
	// everywhere else. The candidates now genuinely include the examples, and the
	// suffix check is what keeps them.
	var all []string
	for _, pat := range []string{
		filepath.Join(root, clusterspec.EnvironmentsDir, "*.yaml*"),
		filepath.Join(root, clusterspec.LandingZoneFile+"*"),
	} {
		m, err := filepath.Glob(pat)
		if err != nil {
			return nil, err
		}
		all = append(all, m...)
	}
	var out []string
	for _, f := range all {
		// Exactly `.yaml`: excludes `.yaml.example` and anything else parked beside
		// a spec (`.yaml.bak`, an editor's swap file) that is not a live spec.
		if filepath.Ext(f) != ".yaml" {
			continue
		}
		if st, err := os.Stat(f); err != nil || st.IsDir() {
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// sweepAplPins applies dropTrackingPin across an instance's environment specs.
// Pure over the filesystem it is given, and it writes only when write is true.
// manageAplVersionSet reports whether any swept spec file hands apl-core's version
// to llz. INSTANCE-WIDE RATHER THAN PER-ENV, because the root's pin reaches an
// opted-in environment by inheritance (mergeCluster falls an absent env value
// through to spec.defaults), so dropping the ROOT pin moves an opted-in deployment
// just as surely as dropping its own.
//
// Read from the same bytes the sweep rewrites rather than from a loaded spec: the
// sweep is a text edit over files that may not parse as a whole LandingZone yet,
// and a loader that refused them would turn a version hazard into an upgrade that
// cannot run at all.
func manageAplVersionSet(files []string) (bool, error) {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			// SKIPPED, NOT FAILED. The sweep proper reads this file too and reports the
			// error there, with whatever it had already written — a pre-pass that
			// errored first discarded that partial result, which is the very reporting
			// gap TestSweepAplPinsReportsWhatItWroteBeforeFailing exists to hold.
			continue
		}
		var doc struct {
			Spec struct {
				Defaults struct {
					Cluster struct {
						Bootstrap struct {
							ManageAplVersion bool `yaml:"manageAplVersion"`
						} `yaml:"bootstrap"`
					} `yaml:"cluster"`
				} `yaml:"defaults"`
				Environments map[string]struct {
					Cluster struct {
						Bootstrap struct {
							ManageAplVersion bool `yaml:"manageAplVersion"`
						} `yaml:"bootstrap"`
					} `yaml:"cluster"`
				} `yaml:"environments"`
				Cluster struct {
					Bootstrap struct {
						ManageAplVersion bool `yaml:"manageAplVersion"`
					} `yaml:"bootstrap"`
				} `yaml:"cluster"`
			} `yaml:"spec"`
		}
		// A FILE THAT DOES NOT PARSE IS LEFT TO THE SWEEP'S OWN HANDLING rather than
		// forced to "owned" here. An unparseable ROOT already defers every env pin
		// (rootUnreadable below), and dropTrackingPin refuses a file it cannot read —
		// so nothing is dropped on the strength of an unreadable spec either way, and
		// short-circuiting here would suppress the deferral's own diagnosis, which
		// names the syntax error as the thing to fix.
		if err := yaml.Unmarshal(b, &doc); err != nil {
			continue
		}
		if doc.Spec.Defaults.Cluster.Bootstrap.ManageAplVersion || doc.Spec.Cluster.Bootstrap.ManageAplVersion {
			return true, nil
		}
		for _, e := range doc.Spec.Environments {
			if e.Cluster.Bootstrap.ManageAplVersion {
				return true, nil
			}
		}
	}
	return false, nil
}

func sweepAplPins(root string, write bool) (AplPinResult, error) {
	var res AplPinResult
	files, err := envSpecFiles(root)
	if err != nil {
		return res, err
	}
	// THE SPEC ROOT GOES FIRST, and the ordering is load-bearing rather than tidy.
	//
	// clusterspec resolves a deployment's pin as pickStr(defaults, env) — mergeCluster
	// in merge.go, not an exported entry point: the env
	// value wins, and an ABSENT env value falls through to spec.defaults — not to
	// the baseline. So if landingzone.yaml keeps a pin of its own, removing an env
	// pin does not make that env track this release; it makes it inherit the
	// default. Measured: a kept `6.0.1` default plus a dropped `v6.2.0` env pin
	// resolves prod to 6.0.1, which is BACKWARD from what prod asserted before —
	// while the sweep printed "it tracks this release's baseline and every future
	// one". A true statement about the file and a false one about the instance.
	//
	// So the root is swept first, and whatever pin survives there governs what the
	// env files may do.
	files = rootFirst(root, files)

	// THE PIN IS LOAD-BEARING WHEN THE INSTANCE OWNS THE VERSION, so nothing is
	// dropped. Every pin found is reported as Refused with the reason, which keeps
	// the upgrade's report honest rather than silently doing nothing.
	owned, err := manageAplVersionSet(files)
	if err != nil {
		return res, err
	}

	rootPin, rootUnreadable := "", false
	for _, f := range files {
		isRoot := f == filepath.Join(root, clusterspec.LandingZoneFile)
		b, err := os.ReadFile(f)
		if err != nil {
			return res, fmt.Errorf("read %s: %w", f, err)
		}
		rel, _ := filepath.Rel(root, f)
		// COUNTED HERE, ABOVE EVERY `continue`. Incrementing further down meant a
		// DEFERRED env was never counted — an undercount masked only because the sole
		// consumer also requires len(Deferred) == 0. A guard that is correct by
		// coincidence is one edit away from being wrong.
		if !isRoot {
			res.Envs++
		}
		out, dropped, refused := dropTrackingPin(string(b))
		if owned && dropped != "" {
			res.Refused = append(res.Refused, AplPinChange{File: rel, Pin: dropped,
				Reason: "spec.cluster.bootstrap.manageAplVersion is set, so this pin is what DEPLOYS — " +
					"dropping it would move the live platform to this release's baseline"})
			continue
		}
		// An env pin we WOULD drop, held back because spec.defaults still pins. The
		// operator has to settle the root before the envs can track anything.
		// `!refused` MATTERS: a refusal also carries a pin in `dropped` (so the report
		// can name it), so keying on `dropped != ""` alone filed an UNREWRITABLE file
		// as merely deferred — with the deferral's remedy, "settle the root first",
		// for a file whose actual problem is that nothing can rewrite it.
		if !isRoot && !refused && dropped != "" && (rootPin != "" || rootUnreadable) {
			// THE TWO CAUSES READ DIFFERENTLY AND HAVE DIFFERENT REMEDIES. A root that
			// still pins is settled by removing that pin; a root that does not PARSE
			// is settled by fixing the syntax — and an unparseable root that never
			// mentions aplChartVersion produced no Kept or Refused entry of its own,
			// so the operator was told "landingzone.yaml still pins too" and handed a
			// step to remove a pin that does not exist, while the actual fault went
			// unnamed.
			why := "the spec root still pins " + rootPin
			if rootUnreadable {
				why = "the spec root does not parse as YAML"
			}
			res.Deferred = append(res.Deferred, AplPinChange{File: rel, Pin: dropped, Reason: why})
			continue
		}
		switch {
		case refused:
			res.Refused = append(res.Refused, AplPinChange{File: rel, Pin: dropped, Reason: refusalReason(string(b))})
		case dropped != "":
			// RECORDED AFTER THE WRITE SUCCEEDS, not before. Appending first meant a
			// failed write still produced the green "no longer pins apl-core X" line
			// for a file that was unchanged and still pinning — the report stating the
			// exact opposite of the tree. The read-failure arm did not cover this; it
			// is the same bug one line further on.
			if write {
				if err := os.WriteFile(f, []byte(out), 0o600); err != nil {
					return res, fmt.Errorf("write %s: %w", f, err)
				}
			}
			res.Dropped = append(res.Dropped, AplPinChange{File: rel, Pin: dropped})
		default:
			if pin := foundAplPin(string(b)); pin != "" {
				res.Kept = append(res.Kept, AplPinChange{File: rel, Pin: pin})
			}
		}
		if !isRoot && foundAplPin(out) == "" {
			// This deployment resolves through spec.defaults, so whatever the root
			// declares is what the gate will actually grade for it.
			res.RootInherited = true
		}
		if isRoot {
			// AN UNREADABLE ROOT IS NOT AN UNPINNED ROOT. foundAplPin swallows the
			// parse error and answers "", so a malformed landingzone.yaml read as "no
			// default pin" and every env pin was deleted underneath it — and once the
			// operator fixes the syntax error, those deployments resolve to the
			// default that was there all along. That is the backward move the
			// rootFirst/Deferred machinery exists to prevent, arriving through the
			// refusal path instead of the kept one. Failing open here defeats the
			// whole ordering.
			if _, err := findPinSites(out); err != nil {
				rootUnreadable = true
			}
			// FROM `out`, NEVER RE-READ FROM DISK. dropTrackingPin returns the
			// post-edit content in BOTH modes, so reading the file back made the dry
			// run answer for a write it had not performed: it said landingzone.yaml
			// "would stop pinning" and, in the same breath, that prod was left alone
			// "because landingzone.yaml still pins too" — plus a next step telling the
			// operator to hand-edit a root the real run retires by itself.
			rootPin = foundAplPin(out)
		}
	}
	return res, nil
}

// rootFirst reorders the spec files so landingzone.yaml is swept before the envs
// that inherit from it.
func rootFirst(root string, files []string) []string {
	lz := filepath.Join(root, clusterspec.LandingZoneFile)
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f == lz {
			out = append(out, f)
		}
	}
	for _, f := range files {
		if f != lz {
			out = append(out, f)
		}
	}
	return out
}

// retrackAplPins is Lever 2's second arm: the environment specs stop restating a
// baseline that has just moved.
//
// A SEAM SO THE WIRING CAN BE PINNED, the same reason the reporters are — a lever
// that silently stops being called is indistinguishable from one with nothing to
// do. TestUpgradeRunRetracksPinsBeforeRendering holds the call.
//
// Returns a NEXT STEPS entry, or "". Never fatal: the upgrade's real work is
// already in the tree by the time this runs, and a spec edit that could abort the
// command would take the whole upgrade with it.
var retrackAplPins = func(dryRun bool) []string {
	tfDir, _, _ := instancelayout.Detect()
	root := filepath.Dir(tfDir)
	if !clusterspec.InstancePresent(root) {
		return nil
	}
	res, sweepErr := sweepAplPins(root, !dryRun)

	// PRINTED BEFORE THE ERROR IS HANDLED, because a sweep that fails halfway has
	// ALREADY REWRITTEN the files it got to. Returning early on the error discarded
	// the partial result, so those edits were in the operator's tree and in no
	// report — the upgrade would have silently changed files it never mentioned.
	for _, d := range res.Dropped {
		if dryRun {
			// Tense matters on a dry run: nothing was written, and reportClobberedManaged
			// above gets this right for the same reason. A past-tense line here would
			// tell the operator an edit had happened that had not.
			fmt.Fprintf(os.Stderr, "%s %s would stop pinning apl-core %s — it would track this release's baseline (%s) and every future one\n",
				color.Cyan("→"), d.File, d.Pin, clusterspec.BaselineAplChartVersion)
			continue
		}
		fmt.Fprintf(os.Stderr, "%s %s no longer pins apl-core %s — it tracks this release's baseline (%s) and every future one\n",
			color.Green("✓"), d.File, d.Pin, clusterspec.BaselineAplChartVersion)
	}

	// REPORTED BEFORE THE ERROR IS RETURNED, all three outcomes. An early return here
	// discarded Kept and Refused as well as Dropped, so a deliberate pin the sweep had
	// already examined — and a duplicate-key file it had already refused — went
	// unmentioned because a LATER file failed to read.
	kept, refused, blocking, deferred := reportAplPinsLeftAlone(res)

	if sweepErr != nil {
		fmt.Fprintf(os.Stderr, "\n%s could not finish re-tracking the apl-core pins: %v\n", color.Yellow("!"), sweepErr)
		if len(res.Dropped) > 0 && !dryRun {
			fmt.Fprintf(os.Stderr, "  %d file(s) were already rewritten, listed above — the sweep stopped partway, so others may still pin.\n", len(res.Dropped))
		}
		return append(aplPinSteps(kept, refused, blocking, deferred),
			"Finish re-tracking spec.cluster.bootstrap.aplChartVersion by hand — this upgrade could not read every spec file")
	}
	// A KEPT PIN IS REPORTED EVEN THOUGH NOTHING WAS DONE TO IT. It is the operator's
	// and stays, but they are the person who has to decide whether holding it is
	// still what they want now the baseline has moved — and the moment to ask is
	// the upgrade that moved it, not a warning they have been scrolling past.
	// EACH REMEDY ITS OWN ENTRY. They were joined with "; " into a single
	// appendStep line, which put "fix this or validation blocks" and "you may want to
	// look at this" in one sentence — undoing the split three lines of comment above
	// argue for.
	return aplPinSteps(kept, refused, blocking, deferred)
}

// reportAplPinsLeftAlone prints every file the sweep did not rewrite and returns the
// two sets, kept apart.
//
// TWO REASONS, TWO REMEDIES. A kept pin is a deliberate choice this upgrade
// respected; a refused file is one the sweep could not safely edit. Collapsing them
// into one checklist line told the operator a refused file "is not one llz set" — a
// statement about the pin's VALUE, when the problem is the file's SHAPE.
func reportAplPinsLeftAlone(res AplPinResult) (kept, refused, blocking, deferred []string) {
	for _, k := range res.Kept {
		// GRADED ON WHAT THE GATE RESOLVES, NOT ON THE FILE IN ISOLATION, and ahead of
		// both blocking arms so neither can skip it. clusterspec's inheritance
		// (mergeCluster in merge.go) lets an env value win, so a default nothing
		// resolves to blocks nothing — while a default nothing overrides does.
		//
		// A pin that blocks gets its OWN slice rather than being folded into `kept`:
		// the stderr line scrolls, and printNextSteps is the last screen, so filing a
		// spec that hard-blocks under an optional "review" is how it goes unread.
		// `len(res.Deferred) == 0` matters: a deferred env keeps its own pin, so it
		// does not inherit — but it is deferred BECAUSE OF this pin, which makes the
		// root the opposite of harmless. Without it the report would call the root
		// overridden in one line and the blocker in the next. The moment the operator
		// followed the deferral advice, that deployment resolved to it and
		// assert-apl-version hard-failed.
		// `res.Envs > 0` MATTERS. With no environments/*.yaml at all, "no env inherits
		// it" is vacuously true — and the pin was filed as an optional Review while
		// the `continue` skipped both blocking arms. The next `llz env add` inherits
		// it and assert-apl-version hard-fails on a spec this command called fine.
		// Nothing overriding it is not the same as every deployment overriding it.
		// OVERRIDDEN TODAY IS NOT HARMLESS TOMORROW, so this NOTES the override and
		// falls through to the blocking arms rather than `continue`-ing past them. A
		// `latest` or `5.0.0` default that every current env overrides was filed as an
		// optional Review — and the next `llz env add` writes no pin, inherits it, and
		// assert-apl-version hard-fails on a spec this command called fine. The
		// zero-env guard beside it covers only the case where there is nothing to
		// override it at all; this is the same hazard one step later.
		if k.File == clusterspec.LandingZoneFile && res.Envs > 0 && !res.RootInherited && len(res.Deferred) == 0 {
			fmt.Fprintf(os.Stderr, "%s %s pins apl-core %s in spec.defaults; every deployment currently overrides it, but a new one added later would inherit it\n",
				color.Yellow("!"), k.File, k.Pin)
		}
		if _, _, _, ok := clusterspec.AplSemver(k.Pin); !ok {
			fmt.Fprintf(os.Stderr, "%s %s pins apl-core %q, which is not a MAJOR.MINOR.PATCH chart version — left alone by this sweep, but it will BLOCK validation; fix or remove it\n",
				color.Yellow("!"), k.File, k.Pin)
			blocking = append(blocking, k.File)
			continue
		}
		// WHAT ACTUALLY BLOCKS IS THE FLOOR, and getting this wrong twice is why it is
		// spelled out here. The delivered preflight is `llz ci assert-apl-version`,
		// which calls clusterspec.AplVersionSupported — a >= MinSupportedAplChartVersion
		// check. clusterspec.aplChartVersionError, the richer major-drift gate whose
		// richer major-drift gate, has NO PRODUCTION CALLER: it and
		// AplChartVersionWarnings are reachable only from their own tests.
		//
		// So a 5.0.0 pin blocks via the FLOOR, which AllowMajorDriftEnv does not
		// release — naming that override as the remedy would send the operator to a
		// switch that cannot help — and a 7.0.0 pin blocks nothing at all. Asking the
		// predicate the delivered gate decides on keeps this true whatever changes.
		if err := clusterspec.AplVersionSupported(k.Pin, "this deployment"); err != nil {
			fmt.Fprintf(os.Stderr, "%s %s pins apl-core %s, below the %s this landing zone supports — left alone by this sweep, but it will BLOCK `llz ci assert-apl-version`; raise or remove it\n",
				color.Yellow("!"), k.File, k.Pin, clusterspec.MinSupportedAplChartVersion)
			blocking = append(blocking, k.File)
			continue
		}
		fmt.Fprintf(os.Stderr, "%s %s pins apl-core %s deliberately (not a version llz has targeted) — left alone; this release targets %s\n",
			color.Yellow("!"), k.File, k.Pin, clusterspec.BaselineAplChartVersion)
		kept = append(kept, k.File)
	}
	for _, d := range res.Deferred {
		fmt.Fprintf(os.Stderr, "%s %s pins apl-core %s and was left alone: %s, so removing this one would resolve the deployment to something other than this release's baseline (%s)\n",
			color.Yellow("!"), d.File, d.Pin, d.Reason, clusterspec.BaselineAplChartVersion)
		deferred = append(deferred, d.File+" ("+d.Reason+")")
	}
	for _, r := range res.Refused {
		// The reason that applies, not a list of candidates: an operator handed the
		// wrong one goes looking for a problem their file does not have.
		fmt.Fprintf(os.Stderr, "%s %s carries an aplChartVersion this upgrade cannot safely rewrite (%s), so it changed nothing there\n",
			color.Yellow("!"), r.File, r.Reason)
		refused = append(refused, r.File+" ("+r.Reason+")")
	}
	return kept, refused, blocking, deferred
}

// aplPinSteps turns the three sets into checklist entries, or none.
func aplPinSteps(kept, refused, blocking, deferred []string) []string {
	var steps []string
	// FIRST, because it is the only one of the three that stops the next command
	// rather than merely deserving a look.
	if len(blocking) > 0 {
		// One slice, two causes — unparseable, and below the supported floor — so the
		// wording covers both rather than asserting one: `5.0.0` IS a
		// MAJOR.MINOR.PATCH version, and blocks for the other reason.
		steps = append(steps, "Fix or remove the aplChartVersion in "+strings.Join(blocking, ", ")+
			": it will BLOCK `llz ci assert-apl-version` (either unparseable, or older than the supported "+
			clusterspec.MinSupportedAplChartVersion+")")
	}
	if len(kept) > 0 {
		steps = append(steps, "Review the apl-core pin in "+strings.Join(kept, ", ")+
			": it is not one llz set, so the upgrade left it at a version this release does not target")
	}
	if len(deferred) > 0 {
		steps = append(steps, "Settle "+clusterspec.LandingZoneFile+" first — "+strings.Join(deferred, ", ")+
			" cannot be retired until it is, or they silently resolve to the default instead of the release baseline")
	}
	if len(refused) > 0 {
		steps = append(steps, "Resolve the aplChartVersion in "+strings.Join(refused, ", ")+
			": the upgrade refused to guess, so those files still pin")
	}
	return steps
}

// foundAplPin returns the pin a file declares, in whatever shape, or "".
//
// It asks the same parser dropTrackingPin does, so the two cannot disagree about
// what counts as a pin — which they did when this was a second regex: a flow-style
// pin was invisible here and landed in none of Dropped/Kept/Refused, and prose in a
// block scalar reported a phantom one that deferred every env forever.
func foundAplPin(content string) string {
	sites, err := findPinSites(content)
	if err != nil {
		return ""
	}
	// THE FIRST SITE IN DOCUMENT ORDER IS NOT NECESSARILY THE PINNING ONE. Returning
	// sites[0] meant a root whose first aplChartVersion is null read as UNPINNED even
	// though a later one really pins — so the deferral was skipped and every env pin
	// was deleted underneath it, resolving those deployments backward the moment the
	// file is loaded. Any non-empty value makes this file "still pins", which is the
	// question every caller is actually asking.
	for _, s := range sites {
		if s.value != "" {
			return s.value
		}
	}
	return ""
}

// ── LOCATING THE PIN: A PARSER DECIDES, TEXT ACTS ────────────────────────────
//
// The DECISION is gopkg.in/yaml.v3's: it already knows what a comment, a quoted
// scalar, a flow mapping and a block scalar are, and a hand-rolled scanner has to
// relearn each one wrongly first.
//
// The EDIT stays textual, spliced at the positions the parser reports, because
// re-serialising the tree would reformat the operator's whole file and discard every
// comment in it — a far worse diff than the one this lever exists to make.

// pinSite is one aplChartVersion key the parser found.
type pinSite struct {
	keyOff, valOff int    // byte offsets of the key and its value
	value          string // the DECODED scalar: `~` and `` both read as ""
	flow           bool   // the enclosing mapping is flow-style
	// anchored is set when the pin, or any mapping enclosing it, carries a YAML
	// anchor. Removing such a line is never a local edit.
	anchored bool
	// headComment is the comment the parser attached ABOVE the key, verbatim. Its
	// PRESENCE is what matters: the sweep refuses such a pin rather than deciding
	// whether the comment describes the pin or the section around it.
	headComment string
	// lineComment marks a trailing comment on the key's own line. Harmless for a
	// block pin (the whole line goes, comment included) and a refusal for a flow one,
	// where splicing a single entry out of a commented line is where the corruption
	// lived.
	lineComment bool
	sameLine    bool // key and value share a line (a plain scalar, not a block)
}

// lineOffsets returns the byte offset at which each 1-based line starts.
func lineOffsets(content string) []int {
	offs := []int{0, 0} // index 0 unused; line 1 starts at 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			offs = append(offs, i+1)
		}
	}
	return offs
}

// nodeOffset converts a yaml.Node's 1-based line/column to a BYTE offset.
//
// yaml.Node.Column counts CHARACTERS, not bytes. Adding it straight to a byte offset
// puts the splice mid-sequence whenever anything non-ASCII sits earlier on the line,
// producing a file that no longer parses while the sweep reports a clean drop. The
// block path is unharmed — its under-count cannot cross a newline — which is why only
// the flow splice shows it.
func nodeOffset(offs []int, content string, n *yaml.Node) int {
	if n.Line <= 0 || n.Line >= len(offs) {
		return -1
	}
	i := offs[n.Line]
	for c := 1; c < n.Column && i < len(content); c++ {
		_, size := utf8.DecodeRuneInString(content[i:])
		i += size
	}
	return i
}

// findPinSites walks the document for every aplChartVersion key in a mapping.
//
// A key inside a block scalar's body is not a key to the parser — it is part of a
// string — so the whole class of "prose that looks like a mapping" disappears here
// rather than needing a guard. Likewise a `#` in a quoted scalar is not a comment,
// and a commented-out pin is not a node.
func findPinSites(content string) ([]pinSite, error) {
	// EVERY DOCUMENT, NOT JUST THE FIRST. yaml.Unmarshal decodes one document and
	// stops without error, so a multi-document spec whose pin lives in the second read
	// as unpinned AND well-formed: rootPin was "", rootUnreadable stayed false, and
	// every env pin was deleted under a root that still pins. That is the fourth
	// distinct route into the same backward resolution — after the ordering, the
	// unreadable root and the null first site — and none of the existing guards
	// covered it, because each of them trusts this function to have looked.
	offs := lineOffsets(content)
	dec := yaml.NewDecoder(strings.NewReader(content))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		docs = append(docs, &doc)
	}
	var out []pinSite
	var walk func(n *yaml.Node, anchored bool)
	walk = func(n *yaml.Node, anchored bool) {
		if n == nil {
			return
		}
		anchored = anchored || n.Anchor != ""
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Value == aplPinKeyName && k.Kind == yaml.ScalarNode {
					// `~`, `null` and an empty value all decode to the null tag, and the
					// loader sees "" for every one of them — same as an omitted key.
					// Node.Value carries the literal text ("~"), so grading that would
					// call a non-pin a pin.
					val := v.Value
					if v.Tag == "!!null" {
						val = ""
					}
					// AN ALIAS REPORTS ITSELF AS ONE. Node.Value carries the alias NAME,
					// so `*ver` read back as the version "ver" and every message that
					// prints a pin said the root "still pins ver". Rendering it as `*ver`
					// keeps the value non-empty — an alias IS a pin, so envs must still
					// defer behind it — while being unmistakable for a chart version in
					// any sentence that quotes it.
					if v.Kind == yaml.AliasNode {
						val = "*" + v.Value
					}
					out = append(out, pinSite{
						keyOff: nodeOffset(offs, content, k),
						valOff: nodeOffset(offs, content, v),
						value:  val,
						flow:   n.Style&yaml.FlowStyle != 0,
						// An anchor anywhere between the document and this key means the
						// line is referenced from elsewhere; see the refusal in
						// dropTrackingPin.
						// An ALIAS value belongs to the same family: `aplChartVersion: *ver`
						// has its real value somewhere else entirely, and v.Value carries
						// the alias NAME — so it was graded as the pin "ver", producing a
						// must-fix step claiming a valid spec "will BLOCK
						// assert-apl-version" and deferring every env behind a root that
						// "still pins ver".
						anchored:    anchored || k.Anchor != "" || v.Anchor != "" || v.Kind == yaml.AliasNode,
						headComment: k.HeadComment,
						lineComment: strings.TrimSpace(v.LineComment) != "" || strings.TrimSpace(k.LineComment) != "",
						// "A PLAIN SCALAR ON THE KEY'S LINE", which is not the same as
						// "starts on the key's line": a literal block scalar
						// (`aplChartVersion: |`) is reported at the `|`, so it shares the
						// key's line while its VALUE lives on the following ones.
						// Deleting just the key line would orphan the body.
						sameLine: v.Kind == yaml.ScalarNode && v.Line == k.Line &&
							v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0,
					})
				}
				walk(v, anchored)
			}
			return
		}
		for _, c := range n.Content {
			walk(c, anchored)
		}
	}
	for _, d := range docs {
		walk(d, false)
	}
	return out, nil
}

// entryEnd returns the offset just past a flow entry's value.
func entryEnd(content string, valOff int) int {
	// COMMENT- AND QUOTE-BLIND WAS FILE CORRUPTION. This scanned for `,`, `}` or a
	// newline and nothing else, so punctuation inside the pin's own trailing comment
	// ended the entry early:
	//
	//	aplChartVersion: v6.2.0 # pinned during rollout, revisit at 6.3
	//
	// spliced to `name: p, revisit at 6.3` — a fabricated key, and clusterspec loads
	// with UnmarshalStrict, so the whole instance spec then fails. `# see {brace` was
	// worse: the depth counter never returned to zero, the scan ran to EOF, and the
	// closing brace and everything after it were deleted. Every variant returned
	// refused=false, so the sweep printed the green "no longer pins" line over a
	// destroyed file.
	//
	depth := 0
	for i := valOff; i < len(content); i++ {
		switch c := content[i]; c {
		case '\'', '"':
			// Skip the scalar wholesale: a `,` or `}` inside it is data, not syntax.
			if end := closingQuote(content, i); end > i {
				i = end
				continue
			}
			return i
		case '#':
			// A comment opens only at a line start or after whitespace, and it runs to
			// the end of the line — so the ENTRY ends here.
			if i == valOff || content[i-1] == ' ' || content[i-1] == '\t' {
				return i
			}
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return i
			}
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		case '\n':
			if depth == 0 {
				return i
			}
		}
	}
	return len(content)
}

// closingQuote returns the index of the quote that closes the scalar opening at
// `open`, or -1 when the line ends first. Handles YAML's doubled ” escape and
// backslash escapes in double quotes.
func closingQuote(content string, open int) int {
	q := content[open]
	for i := open + 1; i < len(content); i++ {
		switch content[i] {
		case '\n':
			return -1
		case '\\':
			if q == '"' {
				i++
			}
		case q:
			if q == '\'' && i+1 < len(content) && content[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

// removeFlowEntry splices out a flow-mapping entry and one separator, so the
// mapping stays well-formed whether the pin was first, last, or the only member.
func removeFlowEntry(content string, s pinSite) string {
	start := s.keyOff
	// `\r` in the trim set, or removing the last entry of a multi-line flow mapping
	// on a CRLF spec leaves a bare LF in an otherwise CRLF file — entryEnd stops at
	// the \n and the \r sits just before it.
	end := strings.TrimRight(content[:entryEnd(content, s.valOff)], " \t\r")
	e := len(end)
	// Prefer taking the separator BEFORE the entry; if there is none (the pin opened
	// the mapping) take the one after it instead.
	i := start - 1
	// `\r` BELONGS IN THIS SET. Without it, a CRLF spec whose flow mapping ends on
	// the pin kept its preceding comma — `bootstrap: {\r\n    name: p,\r\n  }` — which
	// still parses, so nothing caught it. The CRLF test covered only the block path.
	for i >= 0 && (content[i] == ' ' || content[i] == '\t' || content[i] == '\n' || content[i] == '\r') {
		i--
	}
	if i >= 0 && content[i] == ',' {
		start = i
	} else {
		// SKIPS A COMMENT, NOT JUST WHITESPACE. When the pin opens the mapping and its
		// own trailing comment sits between it and the comma, this scan stopped on the
		// `#` and left the separator behind — writing `bootstrap: {\n  , name: p }`,
		// which does not parse, under a green "no longer pins" line. The BACKWARD scan
		// has handled the mirror case since the comment work landed; only the forward
		// arm was blind.
		j := e
		for j < len(content) {
			switch {
			case content[j] == ' ' || content[j] == '\t' || content[j] == '\n':
				j++
			case content[j] == '#':
				if nl := strings.IndexByte(content[j:], '\n'); nl >= 0 {
					j += nl
					continue
				}
				j = len(content)
			default:
				goto separator
			}
		}
	separator:
		if j < len(content) && content[j] == ',' {
			e = j + 1
			// ...and the space that followed it, or removing the FIRST entry leaves
			// `{  managedAppPlatform: true }` with a doubled space — the gratuitous
			// reformat this splice exists to avoid.
			for e < len(content) && (content[e] == ' ' || content[e] == '\t') {
				e++
			}
		}
	}
	out := content[:start] + content[e:]
	out = tidySpliceSite(out, start, strings.TrimSpace(lineAt(content, start)) != "")
	// Collapse only the mapping this removal emptied, anchored on the brace that
	// encloses the removal point — so it works whatever spacing the operator used, and
	// an unrelated mapping on the same line is never reformatted.
	at := min(start, len(out))
	if open := strings.LastIndexByte(out[:at], '{'); open >= 0 {
		if rel := strings.IndexByte(out[open:], '}'); rel >= 0 {
			if inner := out[open+1 : open+rel]; inner != "" && strings.TrimSpace(inner) == "" {
				out = out[:open+1] + out[open+rel:]
			}
		}
	}
	return out
}

// removeBlockLine deletes the line a block-style pin occupies — the key, its value,
// and any trailing comment on that same line, which belongs to it.
//
// It takes nothing above: a comment attached to the key is handled by REFUSING such
// a pin in dropTrackingPin, not here.
func removeBlockLine(content string, s pinSite) string {
	start := strings.LastIndexByte(content[:s.keyOff], '\n') + 1
	end := strings.IndexByte(content[s.keyOff:], '\n')
	if end < 0 {
		return content[:start]
	}
	return content[:start] + content[s.keyOff+end+1:]
}

// lineAt returns the whole line containing the given offset.
func lineAt(content string, off int) string {
	if off > len(content) {
		off = len(content)
	}
	start := strings.LastIndexByte(content[:off], '\n') + 1
	end := strings.IndexByte(content[start:], '\n')
	if end < 0 {
		return content[start:]
	}
	return content[start : start+end]
}

// refusalReason says WHY dropTrackingPin refused, in terms an operator can act on.
//
// Derived by re-asking the parser rather than threaded out of dropTrackingPin: the
// answer is cheap, it cannot drift from the decision (same function, same input),
// and a fourth return value on a predicate used in a dozen tests buys nothing.
func refusalReason(content string) string {
	sites, err := findPinSites(content)
	switch {
	case err != nil:
		return "the file does not parse as YAML"
	case len(sites) > 1:
		return "more than one aplChartVersion key"
	// THE SAME ORDER dropTrackingPin DECIDES IN. These diverged: an anchored pin that
	// ALSO carries a head comment is refused for the ANCHOR up there, and was
	// explained as a comment down here — handing the operator a remedy that does not
	// address what actually stopped it. Two orderings of the same cases is a second
	// copy of the rule, and it drifted the moment one of them gained an arm.
	case len(sites) == 1 && sites[0].anchored:
		return "it involves a YAML anchor or alias, so its value is not local to this line — removing it would strand a reference " +
			"or change every node merged from it"
	case len(sites) == 1 && strings.TrimSpace(sites[0].headComment) != "":
		return "a comment is attached above it, and this sweep will not guess whether that comment describes the pin or the " +
			"section around it — remove the pin by hand, taking or keeping the comment as you mean to"
	case len(sites) == 1 && sites[0].flow && sites[0].lineComment:
		return "it sits in a flow mapping with a trailing comment, which cannot be spliced without guessing where the comment ends"
	case len(sites) == 1 && !sites[0].sameLine:
		return "its value is not a plain scalar on the key's own line"
	case len(sites) == 1 && sites[0].keyOff >= 0 &&
		strings.TrimSpace(lineAt(content, sites[0].keyOff)[:sites[0].keyOff-lineStart(content, sites[0].keyOff)]) != "":
		// The block-sequence entry, and anything else sharing the pin's line ahead of
		// it. Without this arm the operator got "cannot safely rewrite (its shape is
		// not one this upgrade can rewrite)" — a parenthetical restating the sentence
		// around it and naming nothing they can act on.
		return "it does not start its own line — a `- ` sequence indicator or another key sits before it, and deleting the line " +
			"would take that with it"
	case len(sites) == 1 && (sites[0].keyOff < 0 || sites[0].valOff < 0):
		return "the parser could not give a position for it, so there is nothing this sweep can safely cut"
	default:
		return "its shape is not one this upgrade can rewrite"
	}
}

// tidySpliceSite cleans up whatever the removal left on its own line.
//
// TWO OUTCOMES, ONE DECISION, and they were two steps until doing them separately
// broke: the trailing-space trim shifted bytes AHEAD of the index the blank-line
// removal was still using, so it measured the wrong line and left `{\n\n    name: p`.
//
//   - the line is now whitespace-only and was not before: the whole line goes, so a
//     multi-line flow entry does not leave its indentation behind as a blank line.
//   - otherwise: trailing whitespace the removal exposed is trimmed, so cutting an
//     entry together with its line comment does not leave `bootstrap: { ` before the
//     newline.
//
// `hadContent` is measured on the ORIGINAL text, because a line the operator left
// blank is theirs and stays.
func tidySpliceSite(out string, at int, hadContent bool) string {
	if at > len(out) {
		at = len(out)
	}
	ls := lineStart(out, at)
	le := strings.IndexByte(out[ls:], '\n')
	if le < 0 {
		return strings.TrimRight(out, " \t")
	}
	if line := out[ls : ls+le]; strings.TrimSpace(line) == "" && hadContent {
		return out[:ls] + out[ls+le+1:]
	}
	return out[:ls] + strings.TrimRight(out[ls:ls+le], " \t") + out[ls+le:]
}
