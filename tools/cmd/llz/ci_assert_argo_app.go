package main

// ci_assert_argo_app.go implements `llz ci assert-argo-app` — the fail-fast
// gate in front of the bootstrap's 600s pod wait.
//
// Every wedge of the 2026-07-04 outage (PR #142) presented as `llz ci
// wait-pods` blindly burning its full 600s budget on pods whose Argo
// Application was never even created: the platform-bootstrap sync was stuck
// waves earlier, and the log said nothing about why. This command polls for
// the Application to EXIST first — cheap, and on a healthy bootstrap it
// appears within a couple of minutes — and when it doesn't, it fails WITH the
// platform-bootstrap operationState message (the root cause) instead of
// letting the pod wait time out in the dark.
//
// Fail-fast is deliberately conservative: a Running sync whose message says
// "completed unsuccessfully … Retrying" may be a BY-DESIGN first-boot
// transient (SkipDryRunOnMissingResource CRD races recover on retry), so a
// retrying sync only fails this gate at the deadline — with its message.
// Only a terminal phase (Failed/Error: retry budget exhausted, nothing will
// change without intervention) short-circuits immediately.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

func ciAssertArgoAppCmd() *cobra.Command {
	var (
		app       string
		parent    string
		namespace string
		within    int
	)
	cmd := &cobra.Command{
		Use:   "assert-argo-app",
		Short: "fail fast (with the parent sync's operationState) when an Argo Application never appears",
		Long: "Polls for the named Argo CD Application to exist. On a healthy bootstrap the\n" +
			"platform-bootstrap sync creates it within a couple of minutes; when the sync is\n" +
			"wedged the pod wait behind this gate would burn its full budget blind. Exits\n" +
			"non-zero immediately if the parent app's operation is terminally Failed, or at\n" +
			"the deadline — either way printing the parent operationState message (the root\n" +
			"cause). Uses kubectl with the ambient KUBECONFIG.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return assertArgoApp(newAplGateDeps(), namespace, app, parent, time.Duration(within)*time.Second)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "Application that must appear (required)")
	cmd.Flags().StringVar(&parent, "parent", "platform-bootstrap", "app-of-apps whose operationState explains a missing --app")
	cmd.Flags().StringVar(&namespace, "namespace", "argocd", "namespace of the Application resources")
	cmd.Flags().IntVar(&within, "within", 240, "seconds to wait for --app to exist")
	_ = cmd.MarkFlagRequired("app")
	return cmd
}

// gitAuthGrace is how long a git-auth ComparisonError must PERSIST before this
// gate calls it terminal. It exists to absorb one specific race and no other: the
// argocd repo Secret is delivered by an ExternalSecret, so an Application that
// reconciles before it lands can report an auth failure that clears itself. Two
// minutes is comfortably longer than that gap and still ~1/10th of the budget a
// rejected credential used to burn.
const gitAuthGrace = 2 * time.Minute

// assertArgoApp polls (10s cadence, immediate first probe) until app exists,
// the parent operation is terminally Failed/Error, or the deadline passes.
//
// Transient-fetch recovery: the app-of-apps kustomizes a remote base fetched from the
// (public, anonymous) template repo, and that git fetch is intermittently flaky
// ("failed to list refs: repository not found", a git-fetch timeout). A flake leaves
// the parent OutOfSync with a ComparisonError and NO sync operation — Argo CD only
// re-fetches on its slow periodic refresh (~3m), which can outlast this gate. When we
// see such a transient ComparisonError we force an immediate `refresh=hard` (throttled)
// so the re-fetch happens in seconds; a NON-transient ComparisonError (a real manifest
// error) is left alone and surfaces at the deadline as before.
func assertArgoApp(d aplGateDeps, namespace, app, parent string, within time.Duration) error {
	deadline := d.now().Add(within)
	var lastRefresh, firstGitAuth time.Time
	for {
		if _, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", app); ok {
			fmt.Printf("Application %s exists — proceeding to the pod wait.\n", app)
			return nil
		}
		phase, msg := argoOperationState(d, namespace, parent)
		if phase == "Failed" || phase == "Error" {
			fmt.Fprintf(os.Stderr, "::error::Application %s does not exist and %s's sync is terminally %s — nothing will create it without intervention. operationState: %s | %s\n", app, parent, phase, msg, argoParentDiag(d, namespace, parent))
			return fmt.Errorf("%s sync terminally %s before %s was created", parent, phase, app)
		}
		cerr := argoComparisonError(d, namespace, parent)
		// A git-auth refusal is terminal for the same reason it vetoes the phase1
		// downgrade in healthExitCodeState: the remote answered, and nothing in the
		// bootstrap re-mints the credential. Without this the loop merely stops
		// NUDGING (transientFetchError now excludes auth) and still sleeps out the
		// whole window — trading one flavour of dead time for another.
		//
		// Held for gitAuthGrace before aborting, because this gate runs early enough
		// to race the argocd repo Secret's arrival: the Secret comes from an
		// ExternalSecret, and an Application that reconciles in the gap can report an
		// auth failure that a later poll clears on its own. A refusal that outlives
		// the grace is the real thing — the remote has now said no twice, minutes
		// apart. ("repository not found", the other cold-start shape, is excluded
		// from IsGitAuthError entirely and still rides the transient path.)
		if health.IsGitAuthError(cerr) {
			if firstGitAuth.IsZero() {
				firstGitAuth = d.now()
				fmt.Printf("→ %s reports a git-auth failure — holding %s to rule out the repo-Secret race: %s\n", parent, gitAuthGrace, firstLine(cerr))
			} else if d.now().Sub(firstGitAuth) >= gitAuthGrace {
				fmt.Fprintf(os.Stderr, "::error::Application %s does not exist and %s cannot authenticate to the source repo after %s — TERMINAL, polling will not fix a rejected credential. Check APL_VALUES_REPO_TOKEN → otomi.git.password → the argocd repo Secret (it arrives via an ExternalSecret, so it stays empty if external-secrets never installed). ComparisonError: %s | %s\n", app, parent, gitAuthGrace, cerr, argoParentDiag(d, namespace, parent))
				return fmt.Errorf("%s cannot authenticate to the source repo (terminal) before %s was created", parent, app)
			}
		} else {
			firstGitAuth = time.Time{}
		}
		// Force a re-fetch when the parent is wedged on a transient git-fetch flake.
		// Throttled to 20s (a failed fetch returns fast, so a fresh refresh each cycle
		// is safe, but don't hammer): the previous fetch already failed by the time the
		// ComparisonError is visible, so we're kicking a new attempt, not interrupting one.
		if transientFetchError(cerr) && d.now().Sub(lastRefresh) >= 20*time.Second {
			d.kubectl("-n", namespace, "annotate", "application.argoproj.io", parent, "argocd.argoproj.io/refresh=hard", "--overwrite")
			fmt.Printf("→ %s wedged on a transient fetch error — forced a hard refresh to re-fetch: %s\n", parent, firstLine(cerr))
			lastRefresh = d.now()
		}
		if !d.now().Before(deadline) {
			// operationState is empty when the app-of-apps never started a sync
			// (e.g. a child ComparisonError leaves it OutOfSync with no operation),
			// so the real stall reason lives in sync/health/conditions — surface it.
			fmt.Fprintf(os.Stderr, "::error::Application %s still does not exist after %s — the %s sync has not reached its wave. phase=%s operationState: %s | %s\n", app, within, parent, phase, msg, argoParentDiag(d, namespace, parent))
			return fmt.Errorf("%s not created within %s (parent phase %s)", app, within, phase)
		}
		d.sleep(10 * time.Second)
	}
}

// argoOperationState returns the parent Application's operation phase and
// message, best-effort ("" when unreadable — e.g. the parent doesn't exist
// yet either, which the deadline path will surface).
func argoOperationState(d aplGateDeps, namespace, parent string) (phase, message string) {
	out, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", parent,
		"-o", "jsonpath={.status.operationState.phase}{\"\\t\"}{.status.operationState.message}")
	if !ok {
		return "", ""
	}
	parts := strings.SplitN(out, "\t", 2)
	phase = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		message = strings.TrimSpace(parts[1])
	}
	return phase, message
}

// argoParentDiag returns a one-line sync/health/condition summary of the parent
// app-of-apps for the failure annotation. operationState (what
// argoOperationState reports) is empty until a sync OPERATION runs, so a parent
// wedged OutOfSync/Missing on a child ComparisonError shows nothing there — but
// its sync.status, health.status and condition messages carry the real reason.
// Best-effort: returns a hint string when the parent is unreadable.
func argoParentDiag(d aplGateDeps, namespace, parent string) string {
	out, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", parent,
		"-o", "jsonpath={.status.sync.status}/{.status.health.status}{range .status.conditions[*]} [{.type}: {.message}]{end}")
	if out = strings.TrimSpace(out); !ok || out == "" {
		return fmt.Sprintf("%s state unavailable (missing, or cluster unreachable)", parent)
	}
	return fmt.Sprintf("%s sync/health: %s", parent, out)
}

// argoComparisonError returns the parent Application's ComparisonError condition
// message (the "failed to generate manifest …" text), or "" when there is none.
// A ComparisonError means Argo CD could not compute the target state at all —
// distinct from a sync operation failure (argoOperationState).
func argoComparisonError(d aplGateDeps, namespace, parent string) string {
	out, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", parent,
		"-o", `jsonpath={range .status.conditions[?(@.type=="ComparisonError")]}{.message}{end}`)
	if !ok {
		return ""
	}
	return strings.TrimSpace(out)
}

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
func transientFetchError(msg string) bool {
	if msg == "" || health.IsGitAuthError(msg) {
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

// firstLine truncates a multi-line/long condition message to a single readable line
// for the progress log (the full text still lands in the deadline diagnostics).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return strings.TrimSpace(s)
}
