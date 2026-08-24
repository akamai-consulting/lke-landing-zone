package healthsla

// ci_health_sla.go implements the rotation-SLA scheduled check
// `llz ci health-lke-admin-rotation` — the native port of the same-named job in
// llz-scheduled-checks.yml. It reads the newest lke-admin-token Secret's age and
// classifies it with the unit-tested health.ClassifyRotationAge ladder, failing
// the job past its hard critical SLA.
//
// TWO SIBLINGS HAVE BEEN RETIRED, for the same underlying reason: a scheduled
// check is only worth its step if it can reach the thing it measures.
//
//   health-approle-rotation      removed with the AppRole-rotation subsystem —
//                                ESO uses Kubernetes auth now.
//   health-loki-objkey-rotation  removed in #483. It measured
//                                secret/loki/object-store by exec'ing `bao kv
//                                metadata get` with OPENBAO_ROOT_TOKEN, and
//                                bootstrap REVOKES that token — the delivered
//                                workflow declares it `required: false` and says
//                                so. So on every correctly-configured instance
//                                the check took its no-token branch, warned, and
//                                passed; a step labelled "THE GATE" that had
//                                never once been able to fire.
//
//                                Its coverage did not go with it. The reconciler
//                                already samples that exact KV path over
//                                Kubernetes auth and publishes
//                                llz_credential_age_days, and `llz ci
//                                assert-rotation-health` gates that gauge DAILY,
//                                at 90 days rather than 120, failing on an absent
//                                series as well as an overdue one — which the
//                                exec check could not do at all. The test that
//                                holds that replacement to it is
//                                TestLokiObjectStoreIsGatedByTheAgeLane, in
//                                assertions/assertsecrets.
//
// WHY lke-admin SURVIVED the same audit: it reads Secret creationTimestamps from
// kube-system with the kubeconfig the job already holds. Nothing revokes that, so
// it can still measure what it claims to.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
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

	// ItemsOK, NOT Items — and the difference is whether a 90-day CRITICAL SLA can
	// fire at all. A read that fails AFTER Reachable() passed yields zero items,
	// MaxTime reports nothing found, and the function warns and returns nil. So the
	// hard gate over the most privileged credential on the cluster was one RBAC
	// change or one apiserver blip away from being permanently green, reporting
	// "No lke-admin-token Secret found" — a claim about the cluster made without
	// having read it.
	raws, listed := kubectlprobe.ItemsOK("-n", "kube-system", "get", "secrets")
	if !listed {
		fmt.Fprintf(os.Stderr, "::error::could not list kube-system Secrets on %s after the API answered "+
			"Reachable — the lke-admin rotation SLA rendered NO verdict. This is not evidence that the "+
			"credential is fresh, and not evidence that no token exists.\n", reg)
		summary = append(summary, "> **Could not read kube-system Secrets** — no verdict on the lke-admin SLA. Check RBAC.")
		if err := d.Summary("GITHUB_STEP_SUMMARY", summary...); err != nil {
			return err
		}
		return fmt.Errorf("kube-system Secret list unreadable on %s — the lke-admin rotation SLA could not be measured", reg)
	}

	var times []time.Time
	for _, raw := range raws {
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

// rotationVerdict carries the per-check specifics the fail-on-critical SLA tail
// (reportRotationSLA) needs.
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
//
// ONE CALLER since the Loki OBJ-key check was retired (#483), and kept apart from
// it deliberately: this is the half that decides the JOB'S EXIT CODE, and holding
// it separate from the probe is what lets the SLA ladder be read — and changed —
// without reading the Secret-list decode above it.
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
