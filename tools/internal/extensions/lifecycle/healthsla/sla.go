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

	read := lokiObjkeyUpdatedTime(d)
	switch {
	case read.NoToken:
		// THIS GATE IS STRUCTURALLY INERT ON A HEALTHY INSTANCE, and saying so is
		// the honest thing this change can do without breaking every adopter.
		// llz-scheduled-checks.yml declares OPENBAO_ROOT_TOKEN `required: false`
		// and its own comment records that the token is EXPECTED ABSENT because
		// bootstrap revokes it. So on a correctly-configured cluster this check has
		// always taken this branch, warned, and passed — the step is labelled "THE
		// GATE — deliberately no continue-on-error" and has never been able to fire.
		//
		// NOT converted to a hard failure here: that would fail the scheduled run on
		// every correctly-configured instance, which is a worse outcome than the
		// silence and not a decision to make in a bug fix. What it can do is stop
		// reporting the credential as "not found", which is a claim about OpenBao,
		// and report the truth: nothing was measured.
		//
		// THE FIX IS TO STOP NEEDING THE ROOT TOKEN. The reconciler already samples
		// credential ages in-cluster over Kubernetes auth
		// (--reconcile-openbao-gauges); this check should read that gauge rather
		// than exec with a credential the platform deliberately destroys.
		fmt.Fprintf(os.Stderr, "::warning::Loki OBJ key SLA on %s rendered NO verdict: OPENBAO_ROOT_TOKEN is "+
			"unset, which is the EXPECTED steady state (bootstrap revokes it). This gate cannot measure the "+
			"credential's age as written and has not been able to since that revocation was introduced — it is "+
			"not reporting a fresh key, it is reporting nothing.\n", reg)
		summary = append(summary,
			"> **No verdict.** `OPENBAO_ROOT_TOKEN` is unset — the expected steady state, since bootstrap revokes it.",
			"> This check cannot measure the Loki OBJ key's age without it. It must be re-pointed at the",
			"> reconciler's in-cluster credential-age gauge (`--reconcile-openbao-gauges`), which authenticates",
			"> with Kubernetes auth and does not need a root token.")
		return d.Summary("GITHUB_STEP_SUMMARY", summary...)
	case read.ReadFail != nil:
		// We HAD a token and still could not read. That is a real failure of a hard
		// gate, and passing it off as "not bootstrapped" is how a stale credential
		// ages past its SLA unremarked.
		fmt.Fprintf(os.Stderr, "::error::could not read secret/loki/object-store on %s (%v) — the Loki OBJ key "+
			"SLA rendered NO verdict. This is not evidence the key is fresh.\n", reg, read.ReadFail)
		summary = append(summary, fmt.Sprintf("> **Could not read secret/loki/object-store** (%v) — no verdict.", read.ReadFail))
		if err := d.Summary("GITHUB_STEP_SUMMARY", summary...); err != nil {
			return err
		}
		return fmt.Errorf("secret/loki/object-store unreadable on %s — the rotation SLA could not be measured: %w", reg, read.ReadFail)
	case read.NotFound, read.Updated == "":
		fmt.Fprintf(os.Stderr, "::warning::secret/loki/object-store not found on %s — Loki not yet bootstrapped\n", reg)
		summary = append(summary, "> **Action required:** No secret/loki/object-store. Seed it via bootstrap-openbao.yml (docs/runbooks/linode-credential-rotation.md).")
		return d.Summary("GITHUB_STEP_SUMMARY", summary...)
	}
	updated := read.Updated
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

// objkeyRead is what lokiObjkeyUpdatedTime learned, with WHY separated from WHAT.
//
// It used to return a bare "" for three unrelated causes — the token is unset,
// the exec failed, the field is absent — and the caller graded all three as "not
// found: warn and pass". Two of them are "I could not measure", which is not a
// verdict a hard SLA gate is entitled to treat as clean.
type objkeyRead struct {
	Updated string
	NoToken bool // OPENBAO_ROOT_TOKEN unset — see the caller; this is the EXPECTED steady state
	// NotFound is "the path is not there", which `bao` reports by EXITING 2 with
	// "No value found at …" on stderr — not by returning an empty document. Folded
	// into ReadFail it hard-failed the weekly job on an instance that simply has
	// not seeded Loki yet, and left the `Updated == ""` branch — the one carrying
	// the "seed it via bootstrap-openbao.yml" remedy — unreachable against real
	// bao, only satisfiable by a `{"data":{}}` shape nothing produces.
	NotFound bool
	ReadFail error
}

// baoNoValueMarker is how `bao kv metadata get` says "not there". Matched on the
// stderr text because the exit code (2) is shared with usage errors.
const baoNoValueMarker = "no value found at"

// lokiObjkeyUpdatedTime reads secret/loki/object-store's KV-v2 metadata
// updated_time via `bao kv metadata get` inside the OpenBao pod (the same exec
// path `llz openbao exec` uses).
func lokiObjkeyUpdatedTime(d Deps) objkeyRead {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return objkeyRead{NoToken: true}
	}
	argv := d.BaoExecArgv(d.RootPod, token, []string{"kv", "metadata", "get", "-format=json", "secret/loki/object-store"})
	out, err := d.Exec("kubectl", argv...)
	if err != nil {
		// ErrText recovers the child's stderr from the ExitError; without it "no
		// value found" is invisible here and every absence reads as a read failure.
		if strings.Contains(strings.ToLower(kubectlprobe.ErrText(err)), baoNoValueMarker) ||
			strings.Contains(strings.ToLower(string(out)), baoNoValueMarker) {
			return objkeyRead{NotFound: true}
		}
		return objkeyRead{ReadFail: fmt.Errorf("bao kv metadata get: %w", err)}
	}
	var j struct {
		Data struct {
			UpdatedTime string `json:"updated_time"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(out, &j); uerr != nil {
		return objkeyRead{ReadFail: fmt.Errorf("parsing bao kv metadata output: %w", uerr)}
	}
	return objkeyRead{Updated: j.Data.UpdatedTime}
}
