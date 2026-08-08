package baoread

// bao_read.go — kubectl_probe.go for OpenBao KV reads.
//
// baoKVGetField returned "" on any failure. Its own doc comment named the two
// states it was conflating — "'' on any failure (unseeded path, sealed pod)" —
// and every caller then used that "" as PROOF THE PATH IS EMPTY, which is the
// input to a guard whose whole job is to not overwrite a live credential:
//
//	if baoKVGetField(path, field) != "" { skip }   // else: write fresh random bytes
//
// A sealed pod, a revoked token, a konnectivity drop that outlasts the retries —
// each reads as "nothing there", so the guard does not fire and the seeder
// clobbers a credential that is live in-cluster (KV v2 put replaces the whole
// secret, and consumers keep using the old value until they restart). The same
// "" also mints a second Linode object-storage key and a second in-cluster PAT,
// stranding the live ones.
//
// So reads classify, exactly as the kubectl probes do:
//
//	Found   — bao returned a value
//	Absent  — bao ANSWERED: no such path, or no such field. Real evidence.
//	Unknown — no answer: sealed, unreachable, token rejected, exec failed.
//
// Only Absent may be treated as "not seeded". Unknown must fail
// the caller closed — refuse to write — because the cost of guessing wrong is
// destroying a live credential, and the cost of failing is a re-run.

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Verdict is what a KV read learned, if anything.
type Verdict int

const (
	Found         Verdict = iota // a value came back
	Absent                       // bao answered: the path/field is not there
	Unknown                      // no answer at all
	Indeterminate                // the stderr alone does not say which — resolved by a liveness check
)

// absenceMarkers are the bao CLI's own "asked and answered, it is not there"
// messages. Everything else — "Vault is sealed", "permission denied", a
// connection refusal, a konnectivity failure — is NOT evidence of absence.
// Erring toward unknown costs a failed run; erring toward absent costs a
// credential.
var absenceMarkers = []string{
	"no value found",        // "No value found at secret/data/x" — the KV path does not exist
	"field not present",     // the path exists, the field does not
	"not present in secret", // older phrasing of the same
	"no secret found",       // KV v1 phrasing
	"secret not found",      //
}

// deniedMarkers are stderr texts that are definitely NOT about the path: the
// pod is sealed, the token was rejected, or the exec never landed. These are
// unknown outright, without asking anything further.
var deniedMarkers = append([]string{
	"sealed",
	"permission denied",
	"missing client token",
	"invalid token",
	"connection refused",
	"connection reset",
	"i/o timeout",
	"no route to host",
	"server gave http response to https client",
}, lowerAll(TransientMarkers)...)

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// Classify decides whether a failed KV read answered the question.
// Unknown here means "provably not about the path"; a stderr matching
// neither list is INDETERMINATE and resolved by KVGetFieldOK's liveness
// check, because bao's absence phrasing varies across versions and guessing
// "unknown" on a genuinely-unseeded path would fail every cold bootstrap.
func Classify(stderr string) Verdict {
	low := strings.ToLower(stderr)
	// Denials are checked FIRST: a message carrying both a seal/auth complaint
	// and an absence phrase must resolve to unknown, never to absent.
	for _, m := range deniedMarkers {
		if strings.Contains(low, m) {
			return Unknown
		}
	}
	for _, m := range absenceMarkers {
		if strings.Contains(low, m) {
			return Absent
		}
	}
	return Indeterminate
}

// PodUsable reports whether the root pod itself answers and is unsealed. A
// seam so tests drive both sides. This is the tiebreaker: a healthy, unsealed
// pod that refused a KV read is answering ABOUT THE PATH, whereas a pod that
// will not answer its own status tells us nothing about anything.
var PodUsable = func() bool {
	out, _, _ := Exec("", "status", "-format=json")
	return PodStatusUnsealed(out)
}

// KVGetFieldOK reads one field of a KV path and reports what it learned. It
// is the ONLY read helper — the "" -swallowing baoKVGetField described above was
// deleted once its last caller was converted, so the unsafe spelling is no longer
// available to reach for.
func KVGetFieldOK(path, field string) (string, Verdict) {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	out, stderr, err := Exec(token, "kv", "get", "-field="+field, path)
	if err != nil {
		verdict := Classify(stderr)
		if verdict == Indeterminate {
			if PodUsable() {
				return "", Absent
			}
			return "", Unknown
		}
		return "", verdict
	}
	val := strings.TrimSpace(out)
	if val == "" {
		// A clean exit with no output: the field is genuinely empty.
		return "", Absent
	}
	return val, Found
}

// tokenLookupRejectedMarkers are OpenBao's own answers to `token lookup` when
// the token itself is the problem — the only evidence that justifies burning a
// recovery-key quorum to regenerate root.
var tokenLookupRejectedMarkers = []string{
	"permission denied",    // the canonical rejection for a revoked/invalid token
	"invalid token",        //
	"bad token",            //
	"missing client token", //
	"token not found",      //
	"invalid or expired",   //
}

// TokenLookupRejected reports whether OpenBao answered that the token is no
// good, as opposed to the lookup never getting an answer.
func TokenLookupRejected(stderr string) bool {
	low := strings.ToLower(stderr)
	for _, m := range tokenLookupRejectedMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// ErrReadUnknown is the standard fail-closed error for a guard that could not
// read the path it is protecting. Phrased for the operator who finds it in a job
// log: it says what was NOT concluded, so nobody "fixes" it by deleting things.
func ErrReadUnknown(path, field, action string) error {
	return fmt.Errorf("could not read %s (field %q) from OpenBao — this is NOT evidence the path is empty "+
		"(a sealed pod, a rejected token and an exec failure all read the same as unseeded). "+
		"Refusing to %s, because doing so on a live path would overwrite a credential that is in use. "+
		"Check the OpenBao pods are unsealed and OPENBAO_ROOT_TOKEN is valid, then re-run", path, field, action)
}

// ── the seams package main installs ────────────────────────────────────────
//
// TWO CAPABILITIES AND ONE PARSER, and that split is why this package could come
// out at all. bao_read.go reached four package main symbols — ExecFn,
// RootPod, ParsePodStatus and transientExecMarkers — and only the first
// is a capability. The three-clause rule takes the rest apart:
//
//   - `RootPod` is a CONST STRING with six callers in main. Which pod holds
//     root is not this package's business, so the installer bakes it into Exec and
//     the signature here carries only the token and the argv.
//   - `ParsePodStatus` has FOUR other callers in main: shared machinery, not
//     this package's. PodStatusUnsealed is a seam over it — the package asks "is
//     the pod answering and unsealed?" and does not know how that is decided.
//   - `TransientMarkers` is DATA, and it moved HERE rather than being injected. It
//     describes kube/konnectivity transport failures, and the only thing that
//     REASONS about them is the classifier below, which has to tell "the pod
//     answered: no such path" from "nothing answered at all". Same resolution as
//     platform.DeliveredDocs and manifestguard.BootstrapValuePlaceholders: the
//     fact lives once, next to the code that decides on it, and ci_openbao.go
//     imports it.
//
// Defaults are non-nil and inert. PodStatusUnsealed defaults to FALSE on purpose:
// this package's entire discipline is that a non-answer must never read as
// absence, and an uninstalled liveness check that claimed "healthy" would invert
// exactly that.
var (
	// Exec runs `bao <args>` in the root pod with token, returning stdout, stderr
	// and the exec error. Package main bakes in WHICH pod.
	Exec = func(token string, args ...string) (stdout, stderr string, err error) { return "", "", nil }

	// PodStatusUnsealed reports whether `bao status -format=json` output describes
	// a pod that answered and is unsealed.
	PodStatusUnsealed = func(statusJSON string) bool { return false }
)

// ExecStdin is Exec with a stdin payload, for the writes that pipe JSON into the
// in-pod `bao` CLI. Separate rather than a fifth parameter on Exec: every read in
// this package passes an empty stdin, and a signature whose commonest argument is
// always "" invites callers to stop thinking about it.
var ExecStdin = func(token, stdin string, args ...string) (stdout, stderr string, err error) {
	return "", "", fmt.Errorf("baoread: ExecStdin not installed")
}

// Namespace and RootPod name WHERE OpenBao runs.
//
// They are consts in package main with six callers between them, and they moved
// here rather than being injected because they are FACTS, not capabilities — the
// same call platform.DeliveredDocs and manifestguard.BootstrapValuePlaceholders
// got. Anything that talks to OpenBao needs to agree on which pod that is, and the
// only way two copies can disagree is if there are two.
const (
	Namespace = "llz-openbao"
	RootPod   = "platform-openbao-0"
)

// KVPut writes fields to a KV path.
//
// THE WRITE SIDE, AND IT LIVES HERE DESPITE THE PACKAGE NAME. This package is
// named for what it does BEST — classify what a read learned, fail-closed — but
// the seam it needs and the seam a write needs are the same in-pod `bao` CLI, and
// splitting them would mean two packages installing the same exec twice. The read
// classifier is the interesting half; the write is the other end of one wire.
//
// The default ERRORS rather than returning nil. A seeder that silently "succeeded"
// without writing is the failure this whole package's fail-closed discipline
// exists to prevent, and an inert default would reintroduce it one layer up.
var KVPut = kvPut

// kvPut is that default, and it moved here from cmd/llz/ci_harbor.go — where it
// was the LAST straddling seam: defined in the harbor set, consumed by
// ci_objenc.go, baoread_deps.go and (through this var) five other packages.
//
// WHY THIS ONE GETS A REAL DEFAULT AND THE OTHER THREE DO NOT. Exec, ExecStdin and
// PodStatusUnsealed stay installed by package main, because their real bodies
// shell out to kubectl: a test that forgot to stub one would reach whatever
// cluster the developer's context points at, silently, and pass. kvPut cannot do
// that. It requires OPENBAO_ROOT_TOKEN and returns an error when it is unset, so
// an unstubbed call in a test fails loudly for the same reason the erroring
// default did — the discipline is preserved by the token check rather than by the
// stub, which is why the stub is no longer needed.
// It writes through the SAME in-pod `bao` CLI passthrough as `llz openbao exec kv
// put …`. Field values appear only on the kubectl exec argv that passthrough
// already exposes, never on any other local process argv.
func kvPut(path string, fields map[string]string) error {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN must be set (the OpenBao seed writes run through the in-pod bao CLI)")
	}
	args := []string{"kv", "put", path}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic argv
	for _, k := range keys {
		args = append(args, k+"="+fields[k])
	}
	out, errOut, err := ExecFn(RootPod, token, "", args...)
	if err != nil {
		return fmt.Errorf("bao kv put %s: %s", path, strings.TrimSpace(firstNonEmpty(errOut, out)))
	}
	return nil
}

// Install wires the capabilities main owns. Call once, before any read runs.
func Install(exec func(token string, args ...string) (string, string, error), unsealed func(string) bool) {
	Exec, PodStatusUnsealed = exec, unsealed
}

// InstallWrites wires the write half: the stdin-carrying exec.
//
// It used to take the KV put too. It no longer needs to — kvPut lives here now,
// and the only reason main was handing it over was that it lived in
// cmd/llz/ci_harbor.go, three files away from the seam it satisfied.
func InstallWrites(execStdin func(token, stdin string, args ...string) (string, string, error)) {
	ExecStdin = execStdin
}

// TransientMarkers are kube-apiserver/konnectivity transport failures that
// mean the exec stream never reached the pod, so the in-pod `bao` command did
// NOT run — making a retry safe even for non-idempotent ops like `operator
// init`. A real bao error ("already initialized", a sealed-pod status exit)
// carries none of these and is returned on the first try. The canonical case:
// konnectivity reports "No agent available" for up to a few minutes after a
// node/pod comes up — exactly when bao-init first execs — so one blip would
// otherwise fail the entire bootstrap.
var TransientMarkers = []string{
	"No agent available",           // konnectivity: no tunnel agent registered yet
	"error dialing backend",        // apiserver could not dial the kubelet
	"unable to upgrade connection", // SPDY/exec stream never established
	"error sending request",        // request never delivered to the node
	"TLS handshake timeout",        // apiserver↔kubelet handshake stalled
}
