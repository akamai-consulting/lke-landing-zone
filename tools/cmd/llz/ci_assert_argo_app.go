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
	"os/exec"
	"strings"
	"time"

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
			d := aplGateDeps{
				kubectl: func(args ...string) (string, bool) {
					c := exec.Command("kubectl", args...)
					var buf strings.Builder
					c.Stdout, c.Stderr = &buf, &buf
					c.Env = os.Environ()
					return buf.String(), c.Run() == nil
				},
				now:   time.Now,
				sleep: time.Sleep,
			}
			return assertArgoApp(d, namespace, app, parent, time.Duration(within)*time.Second)
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "Application that must appear (required)")
	cmd.Flags().StringVar(&parent, "parent", "platform-bootstrap", "app-of-apps whose operationState explains a missing --app")
	cmd.Flags().StringVar(&namespace, "namespace", "argocd", "namespace of the Application resources")
	cmd.Flags().IntVar(&within, "within", 240, "seconds to wait for --app to exist")
	_ = cmd.MarkFlagRequired("app")
	return cmd
}

// assertArgoApp polls (10s cadence, immediate first probe) until app exists,
// the parent operation is terminally Failed/Error, or the deadline passes.
func assertArgoApp(d aplGateDeps, namespace, app, parent string, within time.Duration) error {
	deadline := d.now().Add(within)
	for {
		if _, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", app); ok {
			fmt.Printf("Application %s exists — proceeding to the pod wait.\n", app)
			return nil
		}
		phase, msg := argoOperationState(d, namespace, parent)
		if phase == "Failed" || phase == "Error" {
			fmt.Fprintf(os.Stderr, "::error::Application %s does not exist and %s's sync is terminally %s — nothing will create it without intervention. operationState: %s\n", app, parent, phase, msg)
			return fmt.Errorf("%s sync terminally %s before %s was created", parent, phase, app)
		}
		if !d.now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "::error::Application %s still does not exist after %s — the %s sync has not reached its wave. phase=%s operationState: %s\n", app, within, parent, phase, msg)
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
