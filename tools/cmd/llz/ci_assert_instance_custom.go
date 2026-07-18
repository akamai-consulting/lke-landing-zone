package main

// ci_assert_instance_custom.go implements `llz ci assert-instance-custom` — the e2e
// gate that proves the operator escape hatch actually WORKS end to end, not merely
// that it renders and passes the static custom_layout checks.
//
// The release-e2e instantiate step seeds a trivial manifest under
// kubernetes-custom/namespaces/<ns>/ before it pushes the instance repo. On a
// converged cluster the instance-custom ApplicationSet's git directory generator must
// then have: discovered that directory, generated the Application
// instance-custom-<ns>, created namespace <ns> (CreateNamespace=true), and synced the
// seeded manifest into it. This asserts that generated Application EXISTS and reaches
// Synced + Healthy — a generation error (an invalid directory name, an unreachable
// repo) or a sync failure reds the e2e instead of shipping a silently-broken hatch.
//
// Why converge / assert-loki do NOT cover this: those gate the PLATFORM Applications.
// An escape hatch that generated nothing (the directory generator matched no path) or
// whose generated App never synced leaves every platform app green — the
// instance-custom-<ns> App simply would not exist. Only an assertion that NAMES the
// generated App catches that. Its ROOT-CAUSE surface differs too: the parent here is
// the instance-custom ApplicationSet (whose health.lua reports ErrorOccurred on a
// generation fault), not platform-bootstrap's operationState — so this carries its own
// diagnostics rather than reusing assert-argo-app's app-of-apps ones.
//
// Read-only; drives the ambient KUBECONFIG through the aplGateDeps seam, so the
// deadline loop is unit-testable without a cluster.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func ciAssertInstanceCustomCmd() *cobra.Command {
	var (
		namespace string
		appSet    string
		within    int
	)
	cmd := &cobra.Command{
		Use:   "assert-instance-custom",
		Short: "fail unless the escape-hatch App instance-custom-<ns> exists and is Synced+Healthy",
		Long: "Proves the operator escape hatch (the instance-custom ApplicationSet syncing\n" +
			"kubernetes-custom/) works end to end. The release-e2e instantiate step seeds a\n" +
			"trivial manifest under kubernetes-custom/namespaces/<ns>/; this polls for the\n" +
			"generated Application instance-custom-<ns> to EXIST (the git directory generator\n" +
			"discovered the dir), then for it to reach Synced + Healthy (namespace created,\n" +
			"manifest applied). A generation fault or a sync failure fails the gate WITH the\n" +
			"ApplicationSet / Application diagnostics that explain it. Uses kubectl with the\n" +
			"ambient KUBECONFIG. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			d := aplGateDeps{
				kubectl: func(args ...string) (string, bool) {
					// runCombined runs the command BEFORE reading its buffer; a
					// `return buf.String(), cmd.Run()==nil` here evaluates buf.String()
					// first (Go left-to-right) and always returns empty output — the
					// bug that made this gate read sync= health= forever.
					c := exec.Command("kubectl", args...)
					c.Env = os.Environ()
					return runCombined(c)
				},
				now:   time.Now,
				sleep: time.Sleep,
			}
			return assertInstanceCustom(d, namespace, appSet, time.Duration(within)*time.Second)
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", "llz-e2e-custom",
		"the kubernetes-custom/namespaces/<ns> basename the release-e2e seed uses; the asserted App is instance-custom-<ns>")
	cmd.Flags().StringVar(&appSet, "appset", "instance-custom",
		"the ApplicationSet whose status explains a generated App that never appeared")
	cmd.Flags().IntVar(&within, "within", 300,
		"seconds to wait for the App to appear AND reach Synced+Healthy (converge already gates it healthy, so this is margin)")
	return cmd
}

// assertInstanceCustom polls (10s cadence, immediate first probe) first until the
// generated Application exists, then until it is Synced + Healthy, both bounded by a
// single `within` deadline. Gating on BOTH Sync==Synced and Health==Healthy matters
// for the trivial-ConfigMap seed: a ConfigMap is health-inert, so a freshly-generated
// App can read Health=Healthy while still Sync=OutOfSync (not yet applied) — requiring
// Synced is what proves the manifest actually landed.
func assertInstanceCustom(d aplGateDeps, namespace, appSet string, within time.Duration) error {
	app := "instance-custom-" + namespace
	deadline := d.now().Add(within)

	// Phase 1: the ApplicationSet git generator must have generated the App.
	for {
		if _, ok := d.kubectl("-n", "argocd", "get", "application.argoproj.io", app); ok {
			break
		}
		if !d.now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "::error::Application %s never appeared within %s — the %s ApplicationSet did not generate it from kubernetes-custom/namespaces/%s/. %s\n",
				app, within, appSet, namespace, appSetDiag(d, appSet))
			return fmt.Errorf("%s not generated within %s", app, within)
		}
		fmt.Printf("waiting for %s to be generated by the %s ApplicationSet…\n", app, appSet)
		d.sleep(10 * time.Second)
	}
	fmt.Printf("Application %s exists — waiting for Synced + Healthy…\n", app)

	// Phase 2: the generated App must sync the seeded manifest and go Healthy.
	for {
		sync, health := argoSyncHealth(d, "argocd", app)
		if sync == "Synced" && health == "Healthy" {
			fmt.Printf("OK: %s sync=%s health=%s — the escape hatch synced the seeded manifest into namespace %s.\n",
				app, sync, health, namespace)
			return nil
		}
		if !d.now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "::error::%s did not reach Synced+Healthy within %s (sync=%s health=%s). %s\n",
				app, within, sync, health, argoAppDiag(d, "argocd", app))
			return fmt.Errorf("%s not Synced+Healthy within %s (sync=%s health=%s)", app, within, sync, health)
		}
		fmt.Printf("  %s sync=%s health=%s — retrying…\n", app, sync, health)
		d.sleep(10 * time.Second)
	}
}

// argoSyncHealth returns the Application's sync and health status ("" when the app is
// unreadable — e.g. it was deleted mid-poll, which the deadline path surfaces). Reads
// the object as JSON and parses the two fields — simpler and less fragile than a
// tab-delimited `-o jsonpath` shape, and the same read `llz ci converge` uses.
func argoSyncHealth(d aplGateDeps, namespace, app string) (sync, health string) {
	out, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", app, "-o", "json")
	if !ok {
		return "", ""
	}
	// The seam folds stderr into out; skip any leading noise before the JSON body.
	if i := strings.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var a struct {
		Status struct {
			Sync   struct{ Status string } `json:"sync"`
			Health struct{ Status string } `json:"health"`
		} `json:"status"`
	}
	if json.Unmarshal([]byte(out), &a) != nil {
		return "", ""
	}
	return a.Status.Sync.Status, a.Status.Health.Status
}

// appSetDiag summarizes the ApplicationSet's status conditions — a generation /
// validation error (ErrorOccurred: an invalid generated App, an unreachable repo)
// surfaces there and explains a generated App that never appeared. Best-effort:
// returns a hint when the ApplicationSet is unreadable.
func appSetDiag(d aplGateDeps, appSet string) string {
	out, ok := d.kubectl("-n", "argocd", "get", "applicationset", appSet,
		"-o", `jsonpath={range .status.conditions[*]}[{.type}: {.status} {.message}]{end}`)
	if out = strings.TrimSpace(out); !ok || out == "" {
		return fmt.Sprintf("%s ApplicationSet state unavailable (missing, or cluster unreachable)", appSet)
	}
	return fmt.Sprintf("%s conditions: %s", appSet, out)
}

// argoAppDiag returns a one-line sync/health/conditions/operationState summary of the
// generated Application for the deadline failure annotation. Best-effort.
func argoAppDiag(d aplGateDeps, namespace, app string) string {
	out, ok := d.kubectl("-n", namespace, "get", "application.argoproj.io", app,
		"-o", `jsonpath={.status.sync.status}/{.status.health.status}{range .status.conditions[*]} [{.type}: {.message}]{end} op={.status.operationState.message}`)
	if out = strings.TrimSpace(out); !ok || out == "" {
		return fmt.Sprintf("%s state unavailable", app)
	}
	return fmt.Sprintf("%s sync/health/conditions: %s", app, out)
}
