package assertplatform

// verdict.go — the DURABLE half of `llz ci assert-k8s-version`: a record of what
// the preflight DECIDED, as distinct from the fact that it ran.
//
// WHY A RECORD AND NOT JUST A MESSAGE (issue #449). Every uncertain arm of this
// gate warns and passes, deliberately: the catalog is a third party's answer over
// the network, and #426 is explicit that a token which 401s on the versions route
// "must not become a hard failure". The cost of that rule is that a pipeline where
// the read ALWAYS fails leaves the gate permanently inert — the exact environment
// the 2026-08-11 incident happened in — and every run of it is green. Nothing
// anywhere measured whether this preflight had ever reached a verdict at all.
//
// A `::warning::` is a person noticing. This is the thing that can be counted: one
// line per run, naming the verdict and whether the ACCOUNT settled it, written to
// stdout and to $GITHUB_STEP_SUMMARY. An e2e cycle whose every record says
// `decided=no` is then a signal rather than a silence.
//
// EMITTED FROM ONE DEFERRED CALL, ON EVERY PATH INCLUDING THE ERRORS. The point is
// fail-closed on vacuity (docs/e2e-gates.md): a run that reports nothing must be
// impossible, so "no record" means the verb did not run rather than "the verb had
// nothing to say". That is also why the zero-value kind renders as UNRECORDED and
// annotates instead of being skipped — a future arm that returns without deciding
// announces itself rather than disappearing.
//
// THE PIN IS PART OF THE RECORD because "decided" without it cannot be checked
// against anything later: two runs a week apart can both say `verdict=offered`
// while the second one is about a version the first would have rejected.

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// k8sVerdictKind is the vocabulary of this preflight's outcomes. Strings rather
// than an int enum so the record is readable in a job log without a decoder ring,
// and so a grep for `verdict=undecided` is the whole query.
type k8sVerdictKind string

const (
	// k8sUnrecorded is the ZERO VALUE, and it is a bug rather than an outcome: a
	// path returned without deciding anything. It is spelled out rather than left
	// implicit because the alternative — treating the empty string as "nothing to
	// report" — is how a gate goes quiet.
	k8sUnrecorded k8sVerdictKind = ""
	// k8sOffered — the account's catalog contains the pin.
	k8sOffered k8sVerdictKind = "offered"
	// k8sNotOffered — the catalog answered and does not contain it. The build fails.
	k8sNotOffered k8sVerdictKind = "not-offered"
	// k8sExempt — not in the catalog, but the deployment's cluster already runs it,
	// so terraform plans no change to k8s_version. See linode.ClusterRunsVersion.
	k8sExempt k8sVerdictKind = "exempt"
	// k8sUndecided — the account could not settle it: unreadable, empty, or too
	// coarse a catalog. THIS IS THE ONE THE RECORD EXISTS FOR.
	k8sUndecided k8sVerdictKind = "undecided"
	// k8sNoSpec — no LandingZone spec here, which is honest in the template repo.
	k8sNoSpec k8sVerdictKind = "no-spec"
	// k8sSpecRejected — the spec side failed before the account was ever asked.
	// Never silent: the build stops on it.
	k8sSpecRejected k8sVerdictKind = "spec-rejected"
)

// decided reports whether THE ACCOUNT settled the question this gate exists to
// ask. Narrower than "the verb produced an outcome" on purpose: a missing spec and
// a rejected one are both outcomes, and neither is evidence that the account can
// build anything. Widening this to mean "something happened" would make the
// measurement agree with the silence it was added to detect.
func (k k8sVerdictKind) decided() bool {
	switch k {
	case k8sOffered, k8sNotOffered, k8sExempt:
		return true
	default:
		return false
	}
}

// k8sVerdict is one run's record.
type k8sVerdict struct {
	env    string
	pin    string
	kind   k8sVerdictKind
	reason string // why it is UNDECIDED (or unusable); empty when the account decided
}

// The reasons an arm can be undecided, as a closed set — a free-text reason is
// not countable, and counting is the whole point.
const (
	reasonCatalogUnreadable = "catalog-unreadable"   // the read failed: network, 401, entitlement
	reasonCatalogEmpty      = "catalog-empty"        // the account reported no versions at all
	reasonCatalogCoarse     = "catalog-too-coarse"   // shapes like `1.33` cannot settle a build id
	reasonNoSpec            = "no-spec-on-disk"      // nothing to check — the template repo
	reasonSpecUnusable      = "spec-does-not-pin-it" // unreadable spec, unknown --env, or no pin
)

// record is the machine-readable line, on stdout so it lands in the step log even
// where $GITHUB_STEP_SUMMARY does not exist (a laptop, a plain CI runner).
func (v k8sVerdict) record() string {
	out := fmt.Sprintf("llz-preflight k8s-version: verdict=%s decided=%s deployment=%q pin=%q",
		v.kindOrUnrecorded(), yesNo(v.kind.decided()), v.env, v.pin)
	if v.reason != "" {
		out += " reason=" + v.reason
	}
	return out
}

// summary is the $GITHUB_STEP_SUMMARY rendering — the run summary is where a
// reader looks when they are asking "did this cycle check anything", so the
// DECIDED/UNDECIDED word leads and the detail follows it.
func (v k8sVerdict) summary() string {
	state := "**UNDECIDED**"
	if v.kind.decided() {
		state = "**decided**"
	}
	line := fmt.Sprintf("- k8sVersion preflight — deployment `%s`, pin `%s`: %s (`%s`)",
		v.env, v.pin, state, v.kindOrUnrecorded())
	if v.reason != "" {
		line += " — " + v.reason
	}
	if !v.kind.decided() {
		line += ". This run did not establish that the account can build this pin."
	}
	return line
}

func (v k8sVerdict) kindOrUnrecorded() k8sVerdictKind {
	if v.kind == k8sUnrecorded {
		return "UNRECORDED"
	}
	return v.kind
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// emitK8sVerdict writes the record everywhere it belongs. It never returns an
// error: a preflight must not fail a build because a summary file could not be
// appended to, and a summary that could not be written is itself reported as an
// annotation rather than swallowed.
func emitK8sVerdict(v k8sVerdict) {
	fmt.Println(v.record())
	if v.kind == k8sUnrecorded {
		fmt.Fprintln(os.Stderr, cigate.Warning(
			"the k8sVersion preflight returned without recording a verdict — this is a bug in `llz ci assert-k8s-version`, "+
				"and it means this run's green step is not evidence that anything was checked."))
	}
	if err := ghaout.Append("GITHUB_STEP_SUMMARY", v.summary()); err != nil {
		fmt.Fprintln(os.Stderr, cigate.Warning(
			"the k8sVersion preflight verdict could not be written to the job summary, so this run's outcome is "+
				"only in the step log: "+cigate.FirstLine(err.Error())))
	}
}
