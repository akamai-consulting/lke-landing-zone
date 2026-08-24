package healthsla

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"

// Deps carries what this package cannot reach for itself.
//
// The cluster GET path is deliberately absent: it goes through
// internal/kubectlprobe, which owns its own Exec seam. Only the capabilities that
// are NOT a plain classified kubectl read appear here.
type Deps struct {
	// Summary appends to a GitHub Actions file (GITHUB_STEP_SUMMARY). It is the
	// only output these checks produce that anything downstream reads, so a
	// fixture MUST actually record what it is handed — a no-op stub turns every
	// summary assertion into a tautology. That has been paid for twice already
	// (teardown's Summary, objenc's SecretField).
	Summary func(envVar string, lines ...string) error

	// BaoExec runs `bao <args>` inside an OpenBao pod, returning stdout/stderr.
	//
	// THE PARAMETER NAMES MATCH baoread.ExecFn EXACTLY, and that is the fix rather
	// than a style choice. This was declared `(pod, addr, token string, ...)` and
	// forwarded positionally into `ExecFn(pod, token, stdin string, ...)` — so
	// `addr` landed in the TOKEN slot and `token` in the STDIN slot. Inert today
	// because the only call site passes "" for both, but the first authenticated
	// caller would have piped its token to the child's stdin and run the command
	// UNAUTHENTICATED, with the token in a place nothing redacts. Identical
	// signatures leave the forwarding with nothing to get wrong.
	// Used for seal status, which is a read.
	BaoExec func(pod, token, stdin string, args ...string) (string, string, error)

	// Reachable reports whether the cluster API answers at all. Separate from the
	// probes because "no cluster" is a legitimate SKIP for a scheduled check —
	// a torn-down cluster or a stale kubeconfig in TF state must not page anyone.
	Reachable func() bool
}

// readyResourceItem is the narrow projection these checks decode: a name, a
// namespace, and Ready conditions.
//
// package main has a wider `meta`-embedding version carrying annotations,
// finalizers and deletionTimestamp — the convergence classifier needs those and
// these checks do not. Two decoders of the same Kubernetes shape is duplication;
// two DIFFERENT PROJECTIONS of it is not, and declaring only the fields you read
// is what keeps a decode honest.
type readyResourceItem struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Conditions []health.Condition `json:"conditions"`
	} `json:"status"`
}
