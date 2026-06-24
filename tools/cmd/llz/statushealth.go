package main

// statushealth.go ports report-argocd-health.sh into `llz status`: classify the
// Argo CD Applications in the current cluster, flag the required support-plane
// ones that are not Synced+Healthy (or missing), and — with --wait — poll until
// they converge or a timeout elapses. The bash version targeted per-region CI
// kubeconfigs under $RUNNER_TEMP; the operator CLI just uses the current kubectl
// context (one cluster), which is what an operator actually has in hand.

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// requiredSupportApps must be Synced+Healthy for the support plane to be up.
var requiredSupportApps = []string{
	"platform-openbao", "platform-harbor", "platform-otel-collector",
	"platform-loki", "platform-prometheus", "platform-grafana",
}

type argoApp struct {
	Name   string
	Sync   string
	Health string
}

func (a argoApp) healthy() bool { return a.Sync == "Synced" && a.Health == "Healthy" }

// classifyArgoApps splits the cluster's Applications into required-unhealthy,
// missing-required, and other-unhealthy — the pure core (unit-tested).
func classifyArgoApps(apps []argoApp, required []string) (reqUnhealthy, missing, otherUnhealthy []string) {
	byName := make(map[string]argoApp, len(apps))
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
		if !a.healthy() {
			reqUnhealthy = append(reqUnhealthy, fmt.Sprintf("%s sync=%s health=%s", a.Name, a.Sync, a.Health))
		}
	}
	for _, a := range apps {
		if reqSet[a.Name] || a.healthy() {
			continue
		}
		otherUnhealthy = append(otherUnhealthy, fmt.Sprintf("%s sync=%s health=%s", a.Name, a.Sync, a.Health))
	}
	return reqUnhealthy, missing, otherUnhealthy
}

// listArgoApps runs `kubectl -n argocd get applications -o json` against the
// current context and parses the Application sync/health states.
func listArgoApps() ([]argoApp, error) {
	out, err := execOutput("kubectl", "-n", "argocd", "get", "applications", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("kubectl get applications: %w", err)
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Sync   struct{ Status string } `json:"sync"`
				Health struct{ Status string } `json:"health"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse applications JSON: %w", err)
	}
	apps := make([]argoApp, 0, len(doc.Items))
	for _, it := range doc.Items {
		apps = append(apps, argoApp{it.Metadata.Name, it.Status.Sync.Status, it.Status.Health.Status})
	}
	return apps, nil
}

// reportArgoHealth prints the Application-health summary for the current context.
// With wait=true it polls every 20s until the required apps converge or timeout
// (seconds) elapses, returning an error if they never do. Without wait it is a
// one-shot report (error if required apps are unhealthy/missing right now).
func reportArgoHealth(g globalOpts, wait bool, timeout int) error {
	if g.dryRun {
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
			fmt.Printf("%s required support-plane Applications are Synced + Healthy\n", green("✓"))
			printList(dim("  other Applications still not healthy:"), otherUnhealthy)
			return nil
		}
		if !wait || time.Now().After(deadline) {
			fmt.Printf("%s required support-plane Applications not Synced/Healthy:\n", red("✗"))
			printList("", reqUnhealthy)
			printList(dim("  missing:"), missing)
			printList(dim("  (other Applications not healthy:)"), otherUnhealthy)
			return fmt.Errorf("%d required Application(s) unhealthy, %d missing", len(reqUnhealthy), len(missing))
		}
		fmt.Printf("%s\n", dim(fmt.Sprintf("  waiting for %d required Application(s) to converge…", len(reqUnhealthy)+len(missing))))
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
		fmt.Println("  " + dim("-") + " " + it)
	}
}
