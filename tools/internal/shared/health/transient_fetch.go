package health

// transient_fetch.go — is an Argo comparison error a TRANSIENT fetch failure, or a
// real one?
//
// Two callers ask, and they must not disagree: `llz ci assert-argo-app` decides
// whether a degraded Application is worth failing a gate over, and the argo-nudge
// reconciler lane decides whether to re-trigger a comparison. One classifying a
// timeout as permanent while the other retries it forever is the kind of
// disagreement that only shows up as a lane that never settles.
//
// It moved here — beside IsGitAuthError, which it already called — when the lane
// was extracted to internal/reconcilelanes and the two callers ended up in
// different packages. This library is where classification rules live precisely so
// there is one of each.

import "strings"

// transientFetchError reports whether msg is a transient git-fetch failure — the
// intermittent flakes an anonymous clone of the template repo throws (the kustomize
// remote-base fetch), which a hard refresh reliably recovers. A real manifest error
// (bad kind, invalid yaml, missing field) matches none of these and is left to fail
// the gate, so recovery never masks a genuine break.
//
// An AUTH refusal is excluded up front, because two of the patterns below —
// "failed to list refs" and "could not read" — match it, and it is the one
// git-fetch failure a hard refresh provably cannot recover: the remote answered,
// the answer was "no", and refreshing asks the identical question again. Before
// this guard, a values-repo credential Argo could not use was re-nudged every
// poll for the full convergence budget.
func IsTransientFetchError(msg string) bool {
	if msg == "" || IsGitAuthError(msg) {
		return false
	}
	m := strings.ToLower(msg)
	for _, p := range []string{
		"failed to list refs", "repository not found", "could not read",
		"timed out", "timeout", "connection refused", "connection reset",
		"tls handshake", "i/o timeout", "dial tcp", "temporary failure",
		"unexpected eof", "remote error", "rpc error",
	} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}
