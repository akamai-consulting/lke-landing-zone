package healthsla

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
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
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

func RunLKEAdminRotation(d Deps, warnDays, criticalDays int) error {
	reg := schedRegion()
	summary := []string{fmt.Sprintf("## lke-admin Rotation SLA — %s — %s", reg, schedStamp()), ""}

	if !d.Reachable() {
		fmt.Fprintf(os.Stderr, "::warning::lke-admin SLA check skipped on %s — cluster API unreachable (no cluster, or stale kubeconfig in TF state)\n", reg)
		summary = append(summary, fmt.Sprintf("> Skipped: cluster API unreachable on `%s`. If a secondary cluster is expected, check terraform-iac-bootstrap/cluster state.", reg))
		return d.Summary("GITHUB_STEP_SUMMARY", summary...)
	}

	var times []time.Time
	for _, raw := range kubectlprobe.Items("-n", "kube-system", "get", "secrets") {
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
		return d.Summary("GITHUB_STEP_SUMMARY", summary...)
	}

	days := health.DaysSince(newest, time.Now())
	ts := newest.Format(time.RFC3339)
	fmt.Printf("Newest lke-admin-token on %s: %s (%d days ago)\n", reg, ts, days)
	return reportRotationSLA(d, summary, rotationVerdict{
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
func reportRotationSLA(d Deps, summary []string, v rotationVerdict) error {
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
	if err := d.Summary("GITHUB_STEP_SUMMARY", summary...); err != nil {
		return err
	}
	if cat == health.CatFail {
		return fmt.Errorf("%s on %s is %dd old — past the %dd Critical SLA", v.noun, v.region, v.days, v.criticalDays)
	}
	return nil
}

func RunLokiObjkeyRotation(d Deps, warnDays, criticalDays int) error {
	reg := schedRegion()
	summary := []string{fmt.Sprintf("## Loki OBJ Key SLA — %s — %s", reg, schedStamp()), ""}

	updated := lokiObjkeyUpdatedTime(d)
	if updated == "" {
		fmt.Fprintf(os.Stderr, "::warning::secret/loki/object-store not found on %s — Loki not yet bootstrapped, or OpenBao unreachable\n", reg)
		summary = append(summary, "> **Action required:** No secret/loki/object-store. Seed it via bootstrap-openbao.yml (docs/runbooks/linode-credential-rotation.md).")
		return d.Summary("GITHUB_STEP_SUMMARY", summary...)
	}
	t, ok := health.ParseExpiryTime(updated)
	if !ok {
		fmt.Fprintf(os.Stderr, "::warning::secret/loki/object-store on %s has an unparseable updated_time %q — verify manually\n", reg, updated)
		summary = append(summary, fmt.Sprintf("> Could not parse updated_time `%s`.", updated))
		return d.Summary("GITHUB_STEP_SUMMARY", summary...)
	}

	days := health.DaysSince(t, time.Now())
	fmt.Printf("secret/loki/object-store on %s last written %s (%d days ago)\n", reg, updated, days)
	return reportRotationSLA(d, summary, rotationVerdict{
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
func lokiObjkeyUpdatedTime(d Deps) string {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return ""
	}
	argv := d.BaoExecArgv(d.RootPod, token, []string{"kv", "metadata", "get", "-format=json", "secret/loki/object-store"})
	out, err := d.Exec("kubectl", argv...)
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
