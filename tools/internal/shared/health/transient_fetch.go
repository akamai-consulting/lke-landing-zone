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

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

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

// ── THREE MORE READS OF WHAT A CLUSTER SAYS ──────────────────────────────────
//
// IsWebhookRace, ArgoComparisonError and LokiConfigText came from three different
// extensions -- kyverno, assert-platform and converge -- and each was imported by
// exactly one peer that wanted only this. They belong beside IsTransientFetchError
// above: all four answer "what is the cluster actually telling us", none of them
// acts, and getting any of them wrong turns a retryable condition into a hard
// failure or the reverse.

// kyvernoWebhookRaceRE matches the transient kyverno-svc admission errors that
// mean "Kyverno is up but its webhook endpoint/cert isn't reachable yet" — a
// 30-90s race that re-running terraform apply clears, so it must not fail the
// whole apply.
var kyvernoWebhookRaceRE = regexp.MustCompile(`failed calling webhook|connect: operation not permitted|connection refused|no endpoints available`)

func IsWebhookRace(out string) bool { return kyvernoWebhookRaceRE.MatchString(out) }

// EXPORTED for cmd/llz/ci_bao_seed_seal_key.go, which surfaces the same
// ComparisonError when a seal-key seed leaves the parent Application unable to
// compare. One reading of "why is Argo stuck", two callers.
//
// ArgoComparisonError returns the parent Application's ComparisonError condition
// message (the "failed to generate manifest …" text), or "" when there is none.
// A ComparisonError means Argo CD could not compute the target state at all —
// distinct from a sync operation failure (argoOperationState).
func ArgoComparisonError(d cigate.Deps, namespace, parent string) string {
	out, ok := d.Kubectl("-n", namespace, "get", "application.argoproj.io", parent,
		"-o", `jsonpath={range .status.conditions[?(@.type=="ComparisonError")]}{.message}{end}`)
	if !ok {
		return ""
	}
	return strings.TrimSpace(out)
}

// lokiConfigText concatenates the data values of every name-matching ConfigMap
// (where the rendered Loki config lives) so the S3 detection can scan it.
func LokiConfigText(match string) string {
	text, _ := LokiConfigTextOK(match)
	return text
}

// LokiConfigTextOK is LokiConfigText with "the cluster answered" reported
// separately, the same split ItemsOK makes and for the same reason.
//
// The empty string has two meanings here — "no ConfigMap matched" and "the list
// could not be read" — and a caller that grades "" as "Loki is not deployed"
// turns an apiserver failure into a pass on the one section that exists to catch
// filesystem-backed Loki. Any caller deciding anything on emptiness must use
// this one; LokiConfigText above is kept for the callers that only render text.
func LokiConfigTextOK(match string) (string, bool) {
	re := regexp.MustCompile(match)
	var b strings.Builder
	items, answered := kubectlprobe.ItemsOK("get", "configmap", "-A")
	if !answered {
		return "", false
	}
	for _, raw := range items {
		var cm struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal(raw, &cm) != nil || !re.MatchString(cm.Metadata.Name) {
			continue
		}
		for _, v := range cm.Data {
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return b.String(), true
}
