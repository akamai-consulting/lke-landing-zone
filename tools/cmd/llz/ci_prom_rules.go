package main

// ci_prom_rules.go implements `llz ci health-prom-rules` — the native port of
// llz-scheduled-checks.yml's Prometheus rule-evaluation check (the last
// scheduled check that was still inline bash + python). A PrometheusRule can
// be syntactically valid yet fail at evaluation time (a missing metric, a
// label-join mistake); Prometheus only exposes that as lastError on
// /api/v1/rules, so this port-forwards the Prometheus pod and inspects every
// rule group. Warn-only, like the other health-* siblings that page via job
// summary annotations rather than blocking scheduled work.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func ciHealthPromRulesCmd() *cobra.Command {
	var localPort, timeout int
	c := &cobra.Command{
		Use:   "health-prom-rules",
		Short: "report PrometheusRule groups with evaluation errors (warn-only)",
		Long: "Native port of the Prometheus rule-evaluation scheduled check. Port-forwards\n" +
			"the llz-observability Prometheus pod, queries /api/v1/rules, and reports any\n" +
			"rule with a lastError to the step summary + ::warning:: annotations —\n" +
			"evaluation failures (missing metric, label-join mistake) that promtool's\n" +
			"syntax check cannot catch. Skips cleanly when no Prometheus pod is found.\n" +
			"Reads REGION for the report headings.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIHealthPromRules(localPort, timeout) },
	}
	c.Flags().IntVar(&localPort, "local-port", 19090, "local port for the Prometheus port-forward")
	c.Flags().IntVar(&timeout, "timeout", 10, "seconds to wait for the port-forward to answer /-/ready")
	return c
}

// promRulesJSON is the slice of the /api/v1/rules response the check reads.
type promRulesJSON struct {
	Data struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Name      string `json:"name"`
				LastError string `json:"lastError"`
			} `json:"rules"`
		} `json:"groups"`
	} `json:"data"`
}

// ruleEvalErrors extracts "group/rule: lastError" lines. Pure, so the
// extraction is unit-testable on canned API responses.
func ruleEvalErrors(body []byte) []string {
	var rules promRulesJSON
	if json.Unmarshal(body, &rules) != nil {
		return nil
	}
	var errs []string
	for _, g := range rules.Data.Groups {
		for _, r := range g.Rules {
			if r.LastError != "" {
				name := r.Name
				if name == "" {
					name = "?"
				}
				errs = append(errs, fmt.Sprintf("%s/%s: %s", g.Name, name, r.LastError))
			}
		}
	}
	return errs
}

// startAttachedPortForward starts a kubectl port-forward that lives only for
// this command (killed by the returned stop func) — unlike harbor-port-forward,
// nothing after this command needs the tunnel. A package var for tests.
var startAttachedPortForward = func(ns, target, ports string) (stop func(), err error) {
	cmd := exec.Command("kubectl", "-n", ns, "port-forward", target, ports)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}, nil
}

func runCIHealthPromRules(localPort, timeout int) error {
	region := os.Getenv("REGION")

	out, err := execOutput("kubectl", "-n", "llz-observability", "get", "pod",
		"-l", "app.kubernetes.io/name=prometheus",
		"-o", "jsonpath={.items[0].metadata.name}")
	pod := strings.TrimSpace(string(out))
	if err != nil || pod == "" {
		fmt.Fprintf(os.Stderr, "::warning::No Prometheus pod found in llz-observability namespace on %s — skipping rule load check\n", region)
		return nil
	}

	stop, err := startAttachedPortForward("llz-observability", pod, fmt.Sprintf("%d:9090", localPort))
	if err != nil {
		return fmt.Errorf("start kubectl port-forward: %w", err)
	}
	defer stop()

	base := fmt.Sprintf("http://localhost:%d", localPort)
	client := &http.Client{Timeout: 5 * time.Second}
	httpOK := func(url string) (body []byte, ok bool) {
		resp, err := client.Get(url)
		if err != nil {
			return nil, false
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, false
		}
		b := make([]byte, 0, 1<<20)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			b = append(b, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		return b, true
	}
	if !waitPoll(time.Duration(timeout)*time.Second, time.Second, func() bool {
		_, ok := httpOK(base + "/-/ready")
		return ok
	}) {
		fmt.Fprintf(os.Stderr, "::warning::Prometheus port-forward did not become ready on %s — skipping rule load check\n", region)
		return nil
	}

	body, ok := httpOK(base + "/api/v1/rules")
	if !ok {
		fmt.Fprintf(os.Stderr, "::warning::could not query Prometheus /api/v1/rules on %s — skipping rule load check\n", region)
		return nil
	}
	errored := ruleEvalErrors(body)

	summary := []string{"", fmt.Sprintf("### Prometheus Rule Evaluation — %s", region), ""}
	if len(errored) == 0 {
		fmt.Printf("All Prometheus rule groups evaluated without errors on %s.\n", region)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			append(summary, "- All rule groups: no evaluation errors")...)
	}
	for _, line := range errored {
		fmt.Fprintf(os.Stderr, "::warning::Rule evaluation error (%s): %s\n", region, line)
	}
	summary = append(summary, "**Rules with evaluation errors:**", "```")
	summary = append(summary, errored...)
	summary = append(summary, "```")
	return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
}
