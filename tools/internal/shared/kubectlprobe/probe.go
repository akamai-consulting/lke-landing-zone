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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Exec is THE shell-out seam, stdout-only — package main's execOutput is a
// closure delegating here, not a rival copy. A package var rather than a Deps
// field because every probe here is a FREE FUNCTION with ten callers, and two of
// them are generic (List/ListOK); threading a receiver through generics buys
// nothing when the only capability is "run kubectl and give me stdout".
//
// Hand it something that works: a nil Exec panics rather than returning an error,
// and the probes below would report the panic as probeUnknown if it did not.
//
// IT ATTACHES STDERR TO THE ERROR, AND THAT MUST STAY HERE rather than in a
// wrapper some entry point installs — a wrapper is present only in the binary that
// installs it and absent from every test of the ~39 packages that delegate here.
// .Output() captures stdout only, and every tool this runs — kubectl, gh, bao,
// tofu — writes its diagnosis to stderr. Without this the caller's %w wrap prints
// `llz: patch llz-openbao/platform-openbao hostAliases: exit status 1:` — a failure
// with its cause structurally discarded — and callers that dutifully append the
// captured output append nothing. Go carries the text on ExitError.Stderr when
// Stderr is unset; wrapped with %w so errors.Is/As still work.
// ── WHY THIS STAYS EXPORTED, AND WILL. ──────────────────────────────────────
//
// It is a package-level mutable var, so any code can call it and any code can
// swap it. That makes it the capability layer's back door: a binding declaring
// `cluster-read` can reach it and run `kubectl delete`, which is why
// capability/seambypass_test.go ratchets every remaining caller.
//
// The obvious next move is to unexport it so capability.For is the only door. IT
// WAS MEASURED AND REFUSED: ~70 assignments across two dozen packages' tests swap
// this var to keep their subjects runnable without a cluster, and that seam is the
// single largest win of the decomposition. Unexporting would trade it for a
// guarantee the ratchet already approximates.
//
// So the enforcement is a counted allowlist rather than the compiler, deliberately,
// and the trade is written down here rather than rediscovered. What WOULD change
// the answer is a test-only swap that does not require export (a build-tagged
// setter, or moving the seam into capability and giving kubectlprobe a
// constructor); neither is worth doing for its own sake, but either would make
// unexporting cheap and both are the shape to reach for if this is revisited.
//
// NOTE THE NAME IS WRONG, TOO. This runs whatever binary it is given — kubectl,
// gh, bao, tofu, ssh-keyscan — and four extensions use it purely for `gh`. Renaming
// it touches several hundred references across ~39 packages, a number that only
// grows, so it has not been done: read "kubectlprobe" as where it started rather
// than what it is.
var Exec = func(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	if err == nil {
		return out, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := bytes.TrimSpace(ee.Stderr); len(msg) > 0 {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

// Combined runs name with args and returns their combined stdout+stderr, ignoring
// exit status. Diagnostics-only, and deliberately not error-gated: on a failure
// path the tool's own text — "No resources found" for an empty namespace, a
// NotFound, a describe's Events block — is the thing worth printing, and all of
// it is what Exec (stdout-only, error-gated) discards.
var Combined = func(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
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
	out, verdict, _ := ProbeDetail(args...)
	return out, verdict
}

// ProbeDetail is Probe, and also hands back WHAT KUBECTL SAID.
//
// BECAUSE "COULD NOT TELL" IS NOT A DIAGNOSIS. Probe collapses every unanswerable
// call — an unset KUBECONFIG, a kubeconfig pointing at a file that is not there, a
// context naming a cluster that is gone, a dead apiserver — into one bare Unknown,
// and a caller that can only print "could not read <object>" reports all four
// identically and names none of them. Measured on the appliability lane: three
// different kubeconfig faults produced byte-identical output that mentioned no
// kubeconfig, no file, and no host, while the arm one layer over — an RBAC denial,
// which DOES print the apiserver's own words — was immediately actionable. The
// text was always there; only the signature threw it away.
func ProbeDetail(args ...string) (out []byte, verdict Verdict, detail string) {
	// UNKNOWN UNTIL SOMETHING SAYS OTHERWISE. Verdict's zero value is Found, so a
	// Retries set to zero would skip the loop and return a FOUND on nil bytes —
	// which ExistsOK reports as exists=true. Retries is an exported package var and
	// callers do lower it (runConverge sets 1), so the floor is not hypothetical.
	verdict = Unknown
	for attempt := 0; attempt < Retries; attempt++ {
		b, err := Exec("kubectl", args...)
		if err == nil {
			return b, Found, ""
		}
		detail = ErrText(err)
		if verdict = ClassifyErr(err); verdict != Unknown {
			return nil, verdict, detail
		}
		if attempt < Retries-1 {
			time.Sleep(Delay)
		}
	}
	return nil, verdict, detail
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
	if IsAbsentText(ErrText(err)) {
		return Absent
	}
	return Unknown
}

// IsAbsentText is ClassifyErr for the callers that hold kubectl's COMBINED output
// rather than an error — the runners that return (output, ok) and throw the error
// away (cigate.RunCombined, and everything built on it).
//
// EXPORTED SO THERE IS ONE LIST. Such a caller cannot reach absenceMarkers, so it
// writes `strings.Contains(out, "NotFound")` instead — a second, shorter list that
// answers differently for "no resources found" and for a CRD that is not
// installed. Both readings of "is it absent" have to come from the same table, or
// one of them will eventually grade an unreachable apiserver as an empty cluster.
func IsAbsentText(out string) bool {
	low := strings.ToLower(out)
	// NOT EVERY "not found" IS THE OBJECT'S. absenceMarkers carries the bare
	// substring "not found" so that `pods "x" not found` classifies, and a kubectl
	// that never ran carries it too:
	//
	//	exec: "kubectl": executable file not found in $PATH
	//	Error in configuration: context was not found for specified context: prod
	//
	// Measured: both made a caller print "statefulset monitoring/loki-ingester does
	// not exist … the fixture step did not run" — a confident, factually false
	// statement about a cluster it never reached. A tooling or kubeconfig fault is
	// the definition of NO ANSWER, so it is excluded before the markers are read and
	// falls through to Unknown, which every caller already treats as "could not tell".
	//
	// WARNING LINES COME OFF FIRST. kubectl writes deprecation notices and admission
	// `Warning:` lines to the same stderr this reads, so a warning that happens to
	// carry one of these phrases must not declassify a genuine NotFound underneath
	// it. Same reasoning, and same shape, as health.IsImmutableFieldRejection.
	//
	// THIS LIST ONLY EVER LOSES ABSENCES, which is the safe direction and the same
	// trade absenceMarkers' own header makes: a misfiled transient costs a retry, a
	// misfiled absence costs a false green. The retry is not free — everything except
	// runConverge runs at Retries=3, Delay=3s — so entries have to earn their place.
	var kept []string
	for _, line := range strings.Split(low, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "warning:") {
			continue
		}
		kept = append(kept, line)
	}
	low = strings.Join(kept, "\n")
	for _, m := range notAnAnswerMarkers {
		if strings.Contains(low, m) {
			return false
		}
	}
	for _, m := range absenceMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// notAnAnswerMarkers are failures that happen BEFORE the apiserver is asked, so
// nothing about them says whether the object exists.
//
// TWO ENTRIES, BECAUSE ONLY TWO EARN THEIR PLACE. An earlier version had seven.
// Measured against real kubectl, four of them changed no verdict for any message
// it can emit: `unable to read client-cert`, `unable to read client-key`,
// `unable to load root certificates` and `no configuration has been provided` all
// arrive spelled "no such file or directory", which is not an absenceMarker, so
// those messages were already Unknown. A fifth, `context was not found`, is
// redundant — kubectl always prefixes it with `Error in configuration:`. Entries
// that change nothing are not free: they are surface for a future message to
// collide with, and they make the list look load-bearing when it is not.
//
// THE exec PREFIX IS ANCHORED ON PURPOSE. `executable file not found` alone is NOT
// unique to a client-side fault — the container runtime emits it and the apiserver
// relays it verbatim: `OCI runtime exec failed: … exec: "test": executable file
// not found in $PATH`. converge probes exactly that shape (a `kubectl exec … test
// -s` on the OpenBao audit log), and it is harmless there today only because
// Exists collapses Absent and Unknown to the same false — a hazard that goes live
// the moment that call becomes ExistsOK. Anchoring to Go's own `exec: "kubectl":`
// prefix keeps this to the case where the binary itself is missing, and
// `oci runtime exec failed` covers the relayed half properly: an exec that never
// started a process has not answered the question either, and that phrase cannot
// occur in a real absence answer.
//
// THE CREDENTIAL-PLUGIN ENTRIES ARE THE WORST CASE THIS LIST EXISTS FOR, and they
// were the one shape it did not have. client-go spells a missing exec plugin
// `getting credentials: exec: executable <NAME> not found` — "executable <name>",
// never "executable file not found" — so neither the bare phrasing nor the
// kubectl-anchored one matches it, and the bare "not found" in absenceMarkers won.
// Measured against real kubectl: `llz ci assert-overlay-applied` then reported
// every mapped object ABSENT, printed five green ticks and "none of the object(s)
// the overlay field map names exist here", and EXITED 0 — a broken kubeconfig
// certified as "this instance does not run Loki". That lane ships to adopters, and
// an OIDC kubelogin / aws-iam-authenticator kubeconfig whose plugin is not
// installed on a laptop or a runner image is exactly how a downstream site meets
// it. Every other fault on that lane exits 1; this one alone passed.
//
// ONE ENTRY, NOT TWO. kubectl also prints "…a client-go credential plugin that is
// not installed" as a friendly hint, and adding both looked like better coverage —
// but a single captured message carries both, so neither would be the SOLE reason
// anything is excluded and the load-bearing test could not tell if either died.
// `getting credentials: exec:` is client-go's own wrapper and is always present.
var notAnAnswerMarkers = []string{
	`exec: "kubectl": executable file not found`, // the kubectl binary is not on PATH
	"error in configuration",                     // kubectl's own kubeconfig-parse failure, context-not-found included
	"oci runtime exec failed",                    // `kubectl exec` never started a process in the container
	"getting credentials: exec:",                 // client-go could not RUN the exec credential plugin
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

// Reachable reports whether the apiserver answers at all.
//
// IT LIVES HERE, not in internal/converge where it was first extracted to, and
// the reason is a bug that extraction caused: `llz ci diagnose-argocd` (package
// main) and `health-sla` both gate on it, and once converge owned it behind
// converge's OWN Exec seam, a test that stubbed main's kubectl no longer stubbed
// this — so diagnose's reachability gate failed against a real cluster and every
// downstream probe assertion came back empty.
//
// Three callers, one capability, and it is a kubectl probe. One seam covers all
// of them precisely because it is the seam this package already owns.
func Reachable() bool {
	_, err := Exec("kubectl", "version", "--request-timeout=10s")
	return err == nil
}

// IT LIVES HERE BECAUSE TWO CALLERS NEEDED IT, and the extraction that moved the
// first one found the second the hard way. `llz ci diagnose-argocd` and the
// cluster bootstrap both ask "which kubeconfig will kubectl actually read"; the
// closure measurement that sized the diagnostic's extraction reads only what the
// moved files REACH, so an inbound caller in package main was invisible until the
// build broke. This package already owns "can kubectl reach the cluster"; which
// file it reads to try is the same question one step earlier.

// EffectiveKubeconfig resolves the kubeconfig kubectl will actually read: the
// $KUBECONFIG env var when it points at a non-empty file, else the default
// ~/.kube/config. Returns "" only when NEITHER exists / is non-empty — the
// genuine "no cluster, nothing to diagnose" signal.
//
// Gating solely on $KUBECONFIG (the previous behavior) silently skipped these
// diagnostics on exactly the failures they exist for: the bootstrap-openbao job
// — like most of our steps — writes ~/.kube/config and relies on kubectl's
// default path, never exporting $KUBECONFIG. So the v0.0.19 (2026-07-05) e2e
// convergence wedge tripped the assert-argo-app gate and then this step
// no-op'd with "No kubeconfig available", losing the platform-bootstrap
// Application state that would have explained the stall.
func EffectiveKubeconfig() string {
	candidates := []string{os.Getenv("KUBECONFIG")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".kube", "config"))
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// Out is Exec with the binary fixed to kubectl and the bytes stringified.
//
// Three lines with nine callers across six packages (four across three when it
// moved), and it lived in
// cmd/llz/verify.go — a file about the `llz verify` command, which is not what a
// kubectl-shaped exec helper is about. It is here so those callers cannot drift
// about whether the error or the output wins.
func Out(args ...string) (string, error) {
	out, err := Exec("kubectl", args...)
	return string(out), err
}

// Lookable reports whether a binary is on PATH.
//
// It goes through LookPathFn rather than exec.LookPath directly so a test can
// answer for a tool the developer's machine happens to have installed — which is
// the whole failure mode a preflight check has: it passes on the laptop that
// wrote it.
var LookPathFn = exec.LookPath

func Lookable(bin string) bool {
	_, err := LookPathFn(bin)
	return err == nil
}
