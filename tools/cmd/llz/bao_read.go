package main

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
//	baoReadFound   — bao returned a value
//	baoReadAbsent  — bao ANSWERED: no such path, or no such field. Real evidence.
//	baoReadUnknown — no answer: sealed, unreachable, token rejected, exec failed.
//
// Only baoReadAbsent may be treated as "not seeded". baoReadUnknown must fail
// the caller closed — refuse to write — because the cost of guessing wrong is
// destroying a live credential, and the cost of failing is a re-run.

import (
	"fmt"
	"os"
	"strings"
)

// baoReadVerdict is what a KV read learned, if anything.
type baoReadVerdict int

const (
	baoReadFound         baoReadVerdict = iota // a value came back
	baoReadAbsent                              // bao answered: the path/field is not there
	baoReadUnknown                             // no answer at all
	baoReadIndeterminate                       // the stderr alone does not say which — resolved by a liveness check
)

// baoAbsenceMarkers are the bao CLI's own "asked and answered, it is not there"
// messages. Everything else — "Vault is sealed", "permission denied", a
// connection refusal, a konnectivity failure — is NOT evidence of absence.
// Erring toward unknown costs a failed run; erring toward absent costs a
// credential.
var baoAbsenceMarkers = []string{
	"no value found",        // "No value found at secret/data/x" — the KV path does not exist
	"field not present",     // the path exists, the field does not
	"not present in secret", // older phrasing of the same
	"no secret found",       // KV v1 phrasing
	"secret not found",      //
}

// baoDeniedMarkers are stderr texts that are definitely NOT about the path: the
// pod is sealed, the token was rejected, or the exec never landed. These are
// unknown outright, without asking anything further.
var baoDeniedMarkers = append([]string{
	"sealed",
	"permission denied",
	"missing client token",
	"invalid token",
	"connection refused",
	"connection reset",
	"i/o timeout",
	"no route to host",
	"server gave http response to https client",
}, lowerAll(transientExecMarkers)...)

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// classifyBaoRead decides whether a failed KV read answered the question.
// baoReadUnknown here means "provably not about the path"; a stderr matching
// neither list is INDETERMINATE and resolved by baoKVGetFieldOK's liveness
// check, because bao's absence phrasing varies across versions and guessing
// "unknown" on a genuinely-unseeded path would fail every cold bootstrap.
func classifyBaoRead(stderr string) baoReadVerdict {
	low := strings.ToLower(stderr)
	// Denials are checked FIRST: a message carrying both a seal/auth complaint
	// and an absence phrase must resolve to unknown, never to absent.
	for _, m := range baoDeniedMarkers {
		if strings.Contains(low, m) {
			return baoReadUnknown
		}
	}
	for _, m := range baoAbsenceMarkers {
		if strings.Contains(low, m) {
			return baoReadAbsent
		}
	}
	return baoReadIndeterminate
}

// baoPodUsable reports whether the root pod itself answers and is unsealed. A
// seam so tests drive both sides. This is the tiebreaker: a healthy, unsealed
// pod that refused a KV read is answering ABOUT THE PATH, whereas a pod that
// will not answer its own status tells us nothing about anything.
var baoPodUsable = func() bool {
	out, _, _ := baoExecFn(rootOpenbaoPod, "", "", "status", "-format=json")
	st, ok := parseBaoPodStatus(out)
	return ok && !st.Sealed
}

// baoKVGetFieldOK reads one field of a KV path and reports what it learned. It
// is the ONLY read helper — the "" -swallowing baoKVGetField described above was
// deleted once its last caller was converted, so the unsafe spelling is no longer
// available to reach for.
func baoKVGetFieldOK(path, field string) (string, baoReadVerdict) {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	out, stderr, err := baoExecFn(rootOpenbaoPod, token, "", "kv", "get", "-field="+field, path)
	if err != nil {
		verdict := classifyBaoRead(stderr)
		if verdict == baoReadIndeterminate {
			if baoPodUsable() {
				return "", baoReadAbsent
			}
			return "", baoReadUnknown
		}
		return "", verdict
	}
	val := strings.TrimSpace(out)
	if val == "" {
		// A clean exit with no output: the field is genuinely empty.
		return "", baoReadAbsent
	}
	return val, baoReadFound
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

// tokenLookupRejected reports whether OpenBao answered that the token is no
// good, as opposed to the lookup never getting an answer.
func tokenLookupRejected(stderr string) bool {
	low := strings.ToLower(stderr)
	for _, m := range tokenLookupRejectedMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// errBaoReadUnknown is the standard fail-closed error for a guard that could not
// read the path it is protecting. Phrased for the operator who finds it in a job
// log: it says what was NOT concluded, so nobody "fixes" it by deleting things.
func errBaoReadUnknown(path, field, action string) error {
	return fmt.Errorf("could not read %s (field %q) from OpenBao — this is NOT evidence the path is empty "+
		"(a sealed pod, a rejected token and an exec failure all read the same as unseeded). "+
		"Refusing to %s, because doing so on a live path would overwrite a credential that is in use. "+
		"Check the OpenBao pods are unsealed and OPENBAO_ROOT_TOKEN is valid, then re-run", path, field, action)
}
