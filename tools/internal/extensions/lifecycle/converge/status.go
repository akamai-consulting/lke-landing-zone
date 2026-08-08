package converge

// statushealth.go ports report-argocd-health.sh into `llz status`: classify the
// Argo CD Applications in the current cluster, flag the required support-plane
// ones that are not Synced+Healthy (or missing), and — with --wait — poll until
// they converge or a timeout elapses. The bash version targeted per-region CI
// kubeconfigs under $RUNNER_TEMP; the operator CLI just uses the current kubectl
// context (one cluster), which is what an operator actually has in hand.

import (
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

// requiredSupportApps must be Synced+Healthy for the support plane to be up.
var requiredSupportApps = []string{
	"platform-openbao", "platform-harbor", "platform-otel-collector",
	"platform-loki", "platform-prometheus", "platform-grafana",
}

// classifyArgoApps splits the cluster's Applications into required-unhealthy,
// missing-required, and other-unhealthy — the pure core (unit-tested).
func classifyArgoApps(apps []health.AppRef, required []string) (reqUnhealthy, missing, otherUnhealthy []string) {
	byName := make(map[string]health.AppRef, len(apps))
	for _, a := range apps {
		byName[a.Name] = a
	}
	reqSet := make(map[string]bool, len(required))
	for _, name := range required {
		reqSet[name] = true
		a, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if !a.Healthy() {
			reqUnhealthy = append(reqUnhealthy, fmt.Sprintf("%s sync=%s health=%s", a.Name, a.Sync, a.Health))
		}
	}
	for _, a := range apps {
		if reqSet[a.Name] || a.Healthy() {
			continue
		}
		otherUnhealthy = append(otherUnhealthy, fmt.Sprintf("%s sync=%s health=%s", a.Name, a.Sync, a.Health))
	}
	return reqUnhealthy, missing, otherUnhealthy
}

// listArgoApps runs `kubectl -n argocd get applications -o json` against the
// current context and parses the Application sync/health states.
func listArgoApps() ([]health.AppRef, error) {
	out, err := deps.Exec("kubectl", "-n", "argocd", "get", "applications", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("kubectl get applications: %w", err)
	}
	apps, err := health.ParseAppRefList(out)
	if err != nil {
		return nil, fmt.Errorf("parse applications JSON: %w", err)
	}
	return apps, nil
}

// ReportArgoHealth prints the Application-health summary for the current context.
// With wait=true it polls every 20s until the required apps converge or timeout
// (seconds) elapses, returning an error if they never do. Without wait it is a
// one-shot report (error if required apps are unhealthy/missing right now).
func ReportArgoHealth(dryRun bool, wait bool, timeout int) error {
	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) kubectl -n argocd get applications -o json (Application health)")
		return nil
	}
	const interval = 20 * time.Second
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)

	for {
		apps, err := listArgoApps()
		if err != nil {
			return err
		}
		reqUnhealthy, missing, otherUnhealthy := classifyArgoApps(apps, requiredSupportApps)

		if len(reqUnhealthy) == 0 && len(missing) == 0 {
			fmt.Printf("%s required support-plane Applications are Synced + Healthy\n", color.Green("✓"))
			printList(color.Dim("  other Applications still not healthy:"), otherUnhealthy)
			return nil
		}
		if !wait || time.Now().After(deadline) {
			fmt.Printf("%s required support-plane Applications not Synced/Healthy:\n", color.Red("✗"))
			printList("", reqUnhealthy)
			printList(color.Dim("  missing:"), missing)
			printList(color.Dim("  (other Applications not healthy:)"), otherUnhealthy)
			return fmt.Errorf("%d required Application(s) unhealthy, %d missing", len(reqUnhealthy), len(missing))
		}
		fmt.Printf("%s\n", color.Dim(fmt.Sprintf("  waiting for %d required Application(s) to converge…", len(reqUnhealthy)+len(missing))))
		time.Sleep(interval)
	}
}

func printList(header string, items []string) {
	if len(items) == 0 {
		return
	}
	if header != "" {
		fmt.Println(header)
	}
	for _, it := range items {
		fmt.Println("  " + color.Dim("-") + " " + it)
	}
}
