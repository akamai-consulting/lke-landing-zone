package main

// kubectl_probe.go — the cluster-probe siblings of guard_corpus.go.
//
// The manifest guards already have doctrine for this bug: requireCorpus exists
// because "a guard that had nothing to check reports the same green as one that
// checked everything". None of that reached the cluster probes, which are the
// converge gate's source of truth.
//
// Every probe here used to collapse each of two very different outcomes into one
// domain value:
//
//	kExists    — any non-zero kubectl exit ⇒ "absent"
//	kItems     — any error ⇒ empty .items[] ⇒ the section is a silent no-op
//	kJSONPath  — any error ⇒ "" ⇒ indistinguishable from an unset field
//
// "The resource is not there" and "we never got an answer" are not the same
// claim, and only the first is evidence. An unreachable API server, an expired
// token, a throttled request or a 10s timeout all read as ABSENT — and absent is
// the input to sections that then skip themselves, report OK, or (worse) tell an
// operator to go delete something.
//
// So probes classify instead of collapsing:
//
//	probeFound   — the call succeeded
//	probeAbsent  — kubectl said NotFound / No resources found / no such resource
//	               type. A real answer: the thing is genuinely not there.
//	probeUnknown — anything else. Not an answer at all.
//
// probeUnknown is retried (a blip usually is one), and if it survives the
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

// probeVerdict is what a kubectl probe learned, if anything.
type probeVerdict int

const (
	probeFound   probeVerdict = iota // the call succeeded
	probeAbsent                      // kubectl answered: the resource is not there
	probeUnknown                     // no answer — unreachable, unauthorized, timed out, throttled
)

// answered reports whether the probe learned anything at all.
func (v probeVerdict) answered() bool { return v != probeUnknown }

// probeRetries / probeDelay bound the retry of a probeUnknown call. Package vars
// so converge can drop the retries (its poll loop is the retry — see runConverge)
// and tests can zero the delay.
var (
	probeRetries = 3
	probeDelay   = 3 * time.Second
)

// kubectlProbe runs `kubectl <args>`, retrying while the failure is one that
// carries no information. Genuine absence is returned on the first attempt —
// re-asking a question kubectl already answered just burns the budget.
func kubectlProbe(args ...string) ([]byte, probeVerdict) {
	var verdict probeVerdict
	for attempt := 0; attempt < probeRetries; attempt++ {
		out, err := execOutput("kubectl", args...)
		if err == nil {
			return out, probeFound
		}
		if verdict = classifyKubectlErr(err); verdict != probeUnknown {
			return nil, verdict
		}
		if attempt < probeRetries-1 {
			time.Sleep(probeDelay)
		}
	}
	return nil, verdict
}

// absenceMarkers are the kubectl stderr texts that mean "asked and answered: it
// is not there". Everything outside this set is treated as no answer, which is
// the safe default — a misfiled transient costs a retry, a misfiled absence
// costs a false green.
var absenceMarkers = []string{
	"notfound",                              // Error from server (NotFound)
	"not found",                             // ...: pods "x" not found
	"no resources found",                    // empty list from a get
	"doesn't have a resource type",          // the CRD is not installed
	"could not find the requested resource", // 404 for a kind that is not served
}

// execErrText is a failed shell-out's diagnostic text: the captured stderr, or
// the error itself when there is none (a stubbed execOutput in tests, or a
// failure before the process ran). execOutput returns stdout only, so without
// this a kubectl failure's actual reason — the apiserver's "No agent available",
// a NotFound — is discarded and the caller is left guessing from an empty stdout.
func execErrText(err error) string {
	if err == nil {
		return ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.TrimSpace(string(ee.Stderr)) != "" {
		return string(ee.Stderr)
	}
	return err.Error()
}

// classifyKubectlErr decides whether a failed kubectl call answered the question.
func classifyKubectlErr(err error) probeVerdict {
	low := strings.ToLower(execErrText(err))
	for _, m := range absenceMarkers {
		if strings.Contains(low, m) {
			return probeAbsent
		}
	}
	return probeUnknown
}

// ── existence ────────────────────────────────────────────────────────────────

// kExistsOK reports whether `kubectl <args>` found the resource, and whether the
// cluster answered at all. Callers whose "absent" branch SKIPS work (or advises
// a destructive fix) must check the second value; callers that hard-fail on
// absence can use kExists.
func kExistsOK(args ...string) (exists, answered bool) {
	_, verdict := kubectlProbe(args...)
	return verdict == probeFound, verdict.answered()
}

// kExists reports whether `kubectl <args>` exits 0. An unanswerable probe reads
// as absent, which is only safe where absence hard-fails.
func kExists(args ...string) bool {
	exists, _ := kExistsOK(args...)
	return exists
}

// ── lists ────────────────────────────────────────────────────────────────────

// kItems runs `kubectl get <args> -o json` and returns its .items[] as raw
// messages, or nil on any error. Routes through the execOutput seam so the
// section orchestrators are unit-testable with stubbed kubectl JSON.
func kItems(args ...string) []json.RawMessage {
	items, _ := kItemsOK(args...)
	return items
}

// kItemsOK is kItems with "the cluster answered" reported separately. A section
// whose corpus comes back empty records nothing and passes — exactly the empty-
// corpus green requireCorpus refuses for the file guards — so any caller that
// would silently skip work must use this and say "inconclusive" instead. See
// scanInventory and sectionItems.
func kItemsOK(args ...string) ([]json.RawMessage, bool) {
	out, verdict := kubectlProbe(append(args, "-o", "json")...)
	if verdict != probeFound {
		return nil, verdict.answered()
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

// kList runs `kubectl get <args> -o json` and decodes its .items[] into T,
// silently dropping any item that does not decode. It is kItems for the common
// case — every section wants typed items, not raw JSON — so the
// unmarshal-and-continue loop lives here once instead of in each of them.
func kList[T any](args ...string) []T { return decodeItems[T](kItems(args...)) }

// kListOK is kList with kItemsOK's answered flag.
func kListOK[T any](args ...string) ([]T, bool) {
	raw, ok := kItemsOK(args...)
	return decodeItems[T](raw), ok
}

// decodeItems decodes already-fetched .items[] into T, dropping what does not
// decode. Split from kList so a caller that needed kItemsOK's success flag can
// still get typed items without a second fetch.
func decodeItems[T any](raws []json.RawMessage) []T {
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

// kJSONPath runs a kubectl get with a -o jsonpath=... arg and returns trimmed
// stdout, or "" when the read failed. "" is also what an unset field returns, so
// a caller that branches on emptiness wants kJSONPathOK.
func kJSONPath(args ...string) string {
	val, _ := kJSONPathOK(args...)
	return val
}

// kJSONPathOK is kJSONPath with "the cluster answered" reported separately. A
// missing resource answers "" (true); an unreadable one answers ("", false).
func kJSONPathOK(args ...string) (string, bool) {
	out, verdict := kubectlProbe(args...)
	if verdict != probeFound {
		return "", verdict.answered()
	}
	return strings.TrimSpace(string(out)), true
}
