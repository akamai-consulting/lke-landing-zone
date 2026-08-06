package kubectlprobe

// Package kubectlprobe is the cluster-probe sibling of internal/guardwalk: the
// classified `kubectl get` every cluster-facing extension needs, in one place.
//
// TEN non-test callers in package main reached for these helpers before the
// extraction — the health family, the assert lanes, the readiness gates, env-set.
// That count is the whole argument for the package: guardwalk was extracted at
// ten and the same threshold applies here. Every remaining cluster-facing
// extraction should now find this done.
//
// probe.go — the cluster-probe siblings of guard_corpus.go.
//
// The manifest guards already have doctrine for this bug: requireCorpus exists
// because "a guard that had nothing to check reports the same color.Green as one that
// checked everything". None of that reached the cluster probes, which are the
// converge gate's source of truth.
//
// Every probe here used to collapse each of two very different outcomes into one
// domain value:
//
//	Exists    — any non-zero kubectl exit ⇒ "absent"
//	Items     — any error ⇒ empty .items[] ⇒ the section is a silent no-op
//	JSONPath  — any error ⇒ "" ⇒ indistinguishable from an unset field
//
// "The resource is not there" and "we never got an answer" are not the same
// claim, and only the first is evidence. An unreachable API server, an expired
// token, a throttled request or a 10s timeout all read as ABSENT — and absent is
// the input to sections that then skip themselves, report OK, or (worse) tell an
// operator to go delete something.
//
// So probes classify instead of collapsing:
//
//	Found   — the call succeeded
//	Absent  — kubectl said NotFound / No resources found / no such resource
//	               type. A real answer: the thing is genuinely not there.
//	Unknown — anything else. Not an answer at all.
//
// Unknown is retried (a blip usually is one), and if it survives the
// retries the *OK siblings report it to the caller so a section can record
// "inconclusive" instead of "none". Sections that hard-fail on absence were
// always safe — a blip there costs a false FAIL, never a false pass — and can
// keep using the plain probes.
//
// secretPresentWithRetry used to be this file, for one call site: it retried the
// phase1 platform-app-ca probe because "a transient API/ACL blip looks identical
// to a genuine NotFound". That was correct and it is now what every probe does,
// so the one-off is gone.

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Exec is the shell-out seam, stdout-only, mirroring package main's execOutput —
// which is what this was before the extraction. A package var rather than a Deps
// field because every probe here is a FREE FUNCTION with ten callers, and two of
// them are generic (List/ListOK); threading a receiver through generics buys
// nothing when the only capability is "run kubectl and give me stdout".
//
// Hand it something that works: a nil Exec panics rather than returning an error,
// and the probes below would report the panic as probeUnknown if it did not.
var Exec = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Verdict is what a kubectl probe learned, if anything.
type Verdict int

const (
	Found   Verdict = iota // the call succeeded
	Absent                 // kubectl Answered: the resource is not there
	Unknown                // no answer — unreachable, unauthorized, timed out, throttled
)

// Answered reports whether the probe learned anything at all.
func (v Verdict) Answered() bool { return v != Unknown }

// Retries / Delay bound the retry of an Unknown call. Package vars
// so converge can drop the retries (its poll loop is the retry — see runConverge)
// and tests can zero the delay.
var (
	Retries = 3
	Delay   = 3 * time.Second
)

// Probe runs `kubectl <args>`, retrying while the failure is one that
// carries no information. Genuine absence is returned on the first attempt —
// re-asking a question kubectl already answered just burns the budget.
func Probe(args ...string) ([]byte, Verdict) {
	var verdict Verdict
	for attempt := 0; attempt < Retries; attempt++ {
		out, err := Exec("kubectl", args...)
		if err == nil {
			return out, Found
		}
		if verdict = ClassifyErr(err); verdict != Unknown {
			return nil, verdict
		}
		if attempt < Retries-1 {
			time.Sleep(Delay)
		}
	}
	return nil, verdict
}

// absenceMarkers are the kubectl stderr texts that mean "asked and answered: it
// is not there". Everything outside this set is treated as no answer, which is
// the safe default — a misfiled transient costs a retry, a misfiled absence
// costs a false color.Green.
var absenceMarkers = []string{
	"notfound",                              // Error from server (NotFound)
	"not found",                             // ...: pods "x" not found
	"no resources found",                    // empty list from a get
	"doesn't have a resource type",          // the CRD is not installed
	"could not find the requested resource", // 404 for a kind that is not served
}

// ErrText is a failed shell-out's diagnostic text: the captured stderr, or
// the error itself when there is none (a stubbed Exec in tests, or a
// failure before the process ran). Exec returns stdout only, so without
// this a kubectl failure's actual reason — the apiserver's "No agent available",
// a NotFound — is discarded and the caller is left guessing from an empty stdout.
func ErrText(err error) string {
	if err == nil {
		return ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(string(ee.Stderr)) != "" {
		return string(ee.Stderr)
	}
	return err.Error()
}

// ClassifyErr decides whether a failed kubectl call answered the question.
func ClassifyErr(err error) Verdict {
	low := strings.ToLower(ErrText(err))
	for _, m := range absenceMarkers {
		if strings.Contains(low, m) {
			return Absent
		}
	}
	return Unknown
}

// ── existence ────────────────────────────────────────────────────────────────

// ExistsOK reports whether `kubectl <args>` found the resource, and whether the
// cluster answered at all. Callers whose "absent" branch SKIPS work (or advises
// a destructive fix) must check the second value; callers that hard-fail on
// absence can use Exists.
func ExistsOK(args ...string) (exists, answered bool) {
	_, verdict := Probe(args...)
	return verdict == Found, verdict.Answered()
}

// Exists reports whether `kubectl <args>` exits 0. An unanswerable probe reads
// as absent, which is only safe where absence hard-fails.
func Exists(args ...string) bool {
	exists, _ := ExistsOK(args...)
	return exists
}

// ── lists ────────────────────────────────────────────────────────────────────

// Items runs `kubectl get <args> -o json` and returns its .items[] as raw
// messages, or nil on any error. Routes through the Exec seam so the
// section orchestrators are unit-testable with stubbed kubectl JSON.
func Items(args ...string) []json.RawMessage {
	items, _ := ItemsOK(args...)
	return items
}

// ItemsOK is Items with "the cluster answered" reported separately. A section
// whose corpus comes back empty records nothing and passes — exactly the empty-
// corpus color.Green requireCorpus refuses for the file guards — so any caller that
// would silently skip work must use this and say "inconclusive" instead. See
// scanInventory and sectionItems.
func ItemsOK(args ...string) ([]json.RawMessage, bool) {
	out, verdict := Probe(append(args, "-o", "json")...)
	if verdict != Found {
		return nil, verdict.Answered()
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(out, &body) != nil {
		// Well-formed exit, unparseable body: we still have no idea what is out
		// there, so this is not an answer either.
		return nil, false
	}
	return body.Items, true
}

// List runs `kubectl get <args> -o json` and decodes its .items[] into T,
// silently dropping any item that does not decode. It is Items for the common
// case — every section wants typed items, not raw JSON — so the
// unmarshal-and-continue loop lives here once instead of in each of them.
func List[T any](args ...string) []T { return DecodeItems[T](Items(args...)) }

// ListOK is List with ItemsOK's answered flag.
func ListOK[T any](args ...string) ([]T, bool) {
	raw, ok := ItemsOK(args...)
	return DecodeItems[T](raw), ok
}

// DecodeItems decodes already-fetched .items[] into T, dropping what does not
// decode. Split from List so a caller that needed ItemsOK's success flag can
// still get typed items without a second fetch.
func DecodeItems[T any](raws []json.RawMessage) []T {
	out := make([]T, 0, len(raws))
	for _, raw := range raws {
		var v T
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// ── field reads ──────────────────────────────────────────────────────────────

// JSONPath runs a kubectl get with a -o jsonpath=... arg and returns trimmed
// stdout, or "" when the read failed. "" is also what an unset field returns, so
// a caller that branches on emptiness wants JSONPathOK.
func JSONPath(args ...string) string {
	val, _ := JSONPathOK(args...)
	return val
}

// JSONPathOK is JSONPath with "the cluster answered" reported separately. A
// missing resource answers "" (true); an unreadable one answers ("", false).
func JSONPathOK(args ...string) (string, bool) {
	out, verdict := Probe(args...)
	if verdict != Found {
		return "", verdict.Answered()
	}
	return strings.TrimSpace(string(out)), true
}
