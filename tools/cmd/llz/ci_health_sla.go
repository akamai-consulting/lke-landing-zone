package main

// ci_health_sla.go implements the rotation-SLA scheduled checks —
// `llz ci health-lke-admin-rotation` and `health-loki-objkey-rotation` — the
// native ports of the same-named jobs in llz-scheduled-checks.yml. Each reads
// the age of a credential (the newest lke-admin-token Secret, the
// secret/loki/object-store metadata) and classifies it with the unit-tested
// health.ClassifyRotationAge ladder; both fail the job past their hard critical
// SLA. (A former health-approle-rotation check was removed with the retired
// AppRole-rotation subsystem — ESO now uses Kubernetes auth.)

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

// schedStamp is the UTC header stamp the scheduled-check summaries print.
func schedStamp() string { return time.Now().UTC().Format("2006-01-02T15:04Z") }

// schedRegion is the deployment label the summaries print (the job's REGION env),
// falling back to "cluster" when unset.
func schedRegion() string {
	if r := os.Getenv("REGION"); r != "" {
		return r
	}
	return "cluster"
}

func ciHealthLKEAdminRotationCmd() *cobra.Command {
	var warnDays, criticalDays int
	c := &cobra.Command{
		Use:   "health-lke-admin-rotation",
		Short: "fail when the newest lke-admin-token Secret breaches the rotation SLA",
		Long: "Native port of the lke-admin-rotation-health scheduled job. Reads the newest\n" +
			"lke-admin-token Secret's age in kube-system and fails the job past --critical-days\n" +
			"(the hard SLA), warning past --warn-days. Skips cleanly when the cluster API is\n" +
			"unreachable (a torn-down cluster, or a stale kubeconfig in TF state).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runHealthLKEAdminRotation(warnDays, criticalDays) },
	}
	c.Flags().IntVar(&warnDays, "warn-days", 35, "warn when the newest token is older than this many days")
	c.Flags().IntVar(&criticalDays, "critical-days", 90, "fail when the newest token is older than this many days (hard SLA)")
	return c
}

func runHealthLKEAdminRotation(warnDays, criticalDays int) error {
	reg := schedRegion()
	summary := []string{fmt.Sprintf("## lke-admin Rotation SLA — %s — %s", reg, schedStamp()), ""}

	if !kubectlReachable() {
		fmt.Fprintf(os.Stderr, "::warning::lke-admin SLA check skipped on %s — cluster API unreachable (no cluster, or stale kubeconfig in TF state)\n", reg)
		summary = append(summary, fmt.Sprintf("> Skipped: cluster API unreachable on `%s`. If a secondary cluster is expected, check terraform-iac-bootstrap/cluster state.", reg))
		return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
	}

	var times []time.Time
	for _, raw := range kItems("-n", "kube-system", "get", "secrets") {
		var s struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw, &s) != nil || !strings.HasPrefix(s.Metadata.Name, "lke-admin-token") {
			continue
		}
		if t, ok := health.ParseExpiryTime(s.Metadata.CreationTimestamp); ok {
			times = append(times, t)
		}
	}

	newest, ok := health.MaxTime(times)
	if !ok {
		fmt.Fprintf(os.Stderr, "::warning::No lke-admin-token Secret found in kube-system on %s — unexpected on LKE-Enterprise\n", reg)
		summary = append(summary, "> **Action required:** No lke-admin-token Secret found. Verify the cluster and see docs/runbooks/lke-admin-rotation.md")
		return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
	}

	days := health.DaysSince(newest, time.Now())
	ts := newest.Format(time.RFC3339)
	fmt.Printf("Newest lke-admin-token on %s: %s (%d days ago)\n", reg, ts, days)
	return reportRotationSLA(summary, rotationVerdict{
		region: reg, noun: "lke-admin", metricLabel: "Newest lke-admin-token", when: ts,
		days: days, warnDays: warnDays, criticalDays: criticalDays,
		fix: fmt.Sprintf("Run secret-rotation.yml → `%s` (docs/runbooks/lke-admin-rotation.md).", reg),
	})
}

// rotationVerdict carries the per-command specifics the shared fail-on-critical
// SLA tail (reportRotationSLA) needs (used by the lke-admin and loki-objkey
// checks, both of which fail past their hard critical SLA).
type rotationVerdict struct {
	region       string
	noun         string // annotation noun, e.g. "lke-admin", "Loki OBJ key"
	metricLabel  string // summary-table label, e.g. "Newest lke-admin-token"
	when         string // formatted timestamp for the metric row
	days         int
	warnDays     int
	criticalDays int
	fix          string // remediation sentence appended to the warn/critical lines
}

// reportRotationSLA renders the standard SLA table, classifies the credential's
// age, emits the per-category annotation + summary line, writes the step
// summary, and returns a non-nil error iff the age breaches the critical SLA.
// Shared by the fail-on-critical rotation checks (lke-admin, loki-objkey).
func reportRotationSLA(summary []string, v rotationVerdict) error {
	summary = append(summary,
		"| Metric | Value |", "|--------|-------|",
		fmt.Sprintf("| %s | %s (%d days ago) |", v.metricLabel, v.when, v.days),
		fmt.Sprintf("| Warn / Critical | %dd / %dd |", v.warnDays, v.criticalDays))

	cat := health.ClassifyRotationAge(v.days, v.warnDays, v.criticalDays)
	switch cat {
	case health.CatFail:
		fmt.Fprintf(os.Stderr, "::error::%s on %s is %dd old — past the %dd Critical SLA. %s\n", v.noun, v.region, v.days, v.criticalDays, v.fix)
		summary = append(summary, fmt.Sprintf("> **CRITICAL:** %dd ≥ %dd SLA breached. %s", v.days, v.criticalDays, v.fix))
	case health.CatWarn:
		fmt.Fprintf(os.Stderr, "::warning::%s on %s is %dd old (≥ %dd) — rotation overdue\n", v.noun, v.region, v.days, v.warnDays)
		summary = append(summary, fmt.Sprintf("> **Action required:** %dd ≥ %dd. %s", v.days, v.warnDays, v.fix))
	default:
		fmt.Printf("%s on %s is current (%dd < %dd).\n", v.noun, v.region, v.days, v.warnDays)
		summary = append(summary, "> Rotation current.")
	}
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", summary...); err != nil {
		return err
	}
	if cat == health.CatFail {
		return fmt.Errorf("%s on %s is %dd old — past the %dd Critical SLA", v.noun, v.region, v.days, v.criticalDays)
	}
	return nil
}

func ciHealthLokiObjkeyRotationCmd() *cobra.Command {
	var warnDays, criticalDays int
	c := &cobra.Command{
		Use:   "health-loki-objkey-rotation",
		Short: "fail when the Loki object-store key in OpenBao breaches the rotation SLA",
		Long: "Native port of the loki-objkey-rotation-health scheduled job. Reads the age of\n" +
			"the secret/loki/object-store version in OpenBao (via kubectl exec bao) and fails\n" +
			"the job past --critical-days (the 120-day Guidelines SLA), warning past\n" +
			"--warn-days. Reads OPENBAO_ROOT_TOKEN; a missing secret/token is a non-fatal warn.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runHealthLokiObjkeyRotation(warnDays, criticalDays) },
	}
	c.Flags().IntVar(&warnDays, "warn-days", 105, "warn when the key is older than this many days")
	c.Flags().IntVar(&criticalDays, "critical-days", 120, "fail when the key is older than this many days (hard SLA)")
	return c
}

func runHealthLokiObjkeyRotation(warnDays, criticalDays int) error {
	reg := schedRegion()
	summary := []string{fmt.Sprintf("## Loki OBJ Key SLA — %s — %s", reg, schedStamp()), ""}

	updated := lokiObjkeyUpdatedTime()
	if updated == "" {
		fmt.Fprintf(os.Stderr, "::warning::secret/loki/object-store not found on %s — Loki not yet bootstrapped, or OpenBao unreachable\n", reg)
		summary = append(summary, "> **Action required:** No secret/loki/object-store. Seed it via bootstrap-openbao.yml (docs/runbooks/linode-credential-rotation.md).")
		return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
	}
	t, ok := health.ParseExpiryTime(updated)
	if !ok {
		fmt.Fprintf(os.Stderr, "::warning::secret/loki/object-store on %s has an unparseable updated_time %q — verify manually\n", reg, updated)
		summary = append(summary, fmt.Sprintf("> Could not parse updated_time `%s`.", updated))
		return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
	}

	days := health.DaysSince(t, time.Now())
	fmt.Printf("secret/loki/object-store on %s last written %s (%d days ago)\n", reg, updated, days)
	return reportRotationSLA(summary, rotationVerdict{
		region: reg, noun: "Loki OBJ key", metricLabel: "Loki OBJ key last reseeded", when: updated,
		days: days, warnDays: warnDays, criticalDays: criticalDays,
		fix: "Rotate the Loki OBJ key (docs/runbooks/linode-credential-rotation.md).",
	})
}

// lokiObjkeyUpdatedTime reads secret/loki/object-store's KV-v2 metadata
// updated_time via `bao kv metadata get` inside the OpenBao pod (the same exec
// path `llz openbao exec` uses). Returns "" when the token is unset, the exec
// fails, or the field is absent — all of which the caller treats as a non-fatal
// "not found" warning, exactly as the job did.
func lokiObjkeyUpdatedTime() string {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return ""
	}
	argv := baoExecArgv(rootOpenbaoPod, token, []string{"kv", "metadata", "get", "-format=json", "secret/loki/object-store"})
	out, err := execOutput("kubectl", argv...)
	if err != nil {
		return ""
	}
	var j struct {
		Data struct {
			UpdatedTime string `json:"updated_time"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &j) != nil {
		return ""
	}
	return j.Data.UpdatedTime
}
