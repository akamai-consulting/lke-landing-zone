package main

// ci_assert_rotation_health.go implements `llz ci assert-rotation-health` — the
// gate on the credential-rotation lifecycle.
//
// THE COUPLING IT GUARDS. reconcilelanes.CredPaths (reconcile_openbao.go) DECLARES every
// credential whose rotation age is tracked, with the class that says whether
// anything will ever lower that age. The openbao-gauges lane SAMPLES those paths
// and publishes llz_credential_age_days{cred,class}. LLZCredentialRotationOverdue
// then alerts on the gauge. Three components, one contract, and the failure mode
// is a credential that is DECLARED but publishes NO SERIES: it vanishes from the
// single pane, and — this is the part that matters — no alert can ever fire for
// it, because an alert on an absent series is an alert that never evaluates.
//
// That is not hypothetical. The `static` class exists because those paths
// "published NO series at all and were invisible on the single pane rather than
// visibly old" (reconcilelanes.CredClassStatic's own comment). A silently-missing series is the
// native failure of this subsystem, and nothing gated it.
//
// WHAT IT ASSERTS. Two lanes, because the credential single pane has two feeds
// and they fail in different ways.
//
// AGE LANE — for every credential in reconcilelanes.CredPaths whose class is ALERTABLE
// (automated / on-demand — the classes something is expected to rotate):
//
//   1. a llz_credential_age_days series exists for it, and
//   2. its age is within the SLA its class carries.
//
// PRESENCE LANE — for the GitHub-held credentials in ghSecretTargets, where the
// failure is not "old" but "not there". That whole feed was ungated: the age lane
// reads reconcilelanes.CredPaths, so it had nothing to say about a write-time probe that never
// authenticated (which is what was happening in production), a credential that
// was never configured, or a root token left set after a break-glass. See
// evalPresenceHealth.
//
// Non-alertable classes (generate-once / tracks-source / static) are REPORTED,
// never gated: nothing will lower their age, so failing on it would be a
// permanent red that trains people to ignore the gate — the same reasoning that
// keeps them off the 90d alert.
//
// WHAT IT DELIBERATELY DOES NOT DO: force a rotation. `assert-broad-pat-rotation`
// already exercises one full mint → OpenBao → publish → revoke cycle end to end,
// and it is safe only because an e2e-unique label and BROAD_PAT_DEPLOYMENTS=e2e
// confine the mint and revoke to a throwaway PAT family. The other credentials
// have no such containment:
//
//   lke-admin         rotating it deletes the kubeconfig the running job is USING
//   obj-key           rotating it mid-run can cut TF state access
//   db-admin          Linode resets the Managed Postgres password IN PLACE with no
//                     overlap window, so every consumer breaks until ESO re-syncs
//                     (which is exactly why it is on-demand and not on a cron)
//   state-passphrase  a near one-way door on the state encryption
//
// Forcing those inside an e2e run would be a gate that damages the thing it is
// measuring. The age assertion catches what actually goes wrong with them — a
// rotator that stopped, or an operator-triggered rotation nobody triggered —
// without staging an outage to prove it.
//
// Read-only.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reconcilelanes"
)

// Rotation SLAs, in days. These MUST match the LLZCredentialRotationOverdue /
// LLZCredentialNeverRotated expressions in
// platform-apl/components/llzReconciler/llz-reconciler/prometheusrule.yaml —
// TestRotationSLAsMatchThePrometheusRules pins them. A gate that disagreed with
// the alert would either fail on credentials nobody is being paged about, or
// pass on ones they are.
const (
	rotationSLAAlertableDays = 90
	rotationSLAInfoDays      = 365
)

// alertableCredClasses are the classes something is expected to rotate, and so
// the only ones whose age can fairly gate. Kept in step with the alert's
// class=~"automated|on-demand" matcher.
var alertableCredClasses = map[string]bool{
	reconcilelanes.CredClassAutomated: true,
	reconcilelanes.CredClassOnDemand:  true,
}

func ciAssertRotationHealthCmd() *cobra.Command {
	var prom, namespace string
	var settle, interval int
	var strict, requireInventory bool
	c := &cobra.Command{
		Use:   "assert-rotation-health",
		Short: "fail unless every rotatable credential is being observed and is within its rotation SLA",
		Long: "Gates the credential-rotation lifecycle. For every credential reconcilelanes.CredPaths declares\n" +
			"with an ALERTABLE class (automated / on-demand), asserts a\n" +
			"llz_credential_age_days series exists AND its age is within the class SLA.\n\n" +
			"The missing series is the point. reconcilelanes.CredPaths declares the credential, the\n" +
			"openbao-gauges lane samples it, and LLZCredentialRotationOverdue alerts on the\n" +
			"result — so a credential that is declared but publishes nothing disappears from\n" +
			"the single pane AND can never fire an alert, because a rule over an absent\n" +
			"series never evaluates. That is the native failure of this subsystem (the\n" +
			"`static` class exists because a whole group of paths had silently published no\n" +
			"series at all) and nothing gated it.\n\n" +
			"Non-alertable classes (generate-once / tracks-source / static) are reported,\n" +
			"never gated: nothing will ever lower their age, so failing on it would be a\n" +
			"permanent red. --strict also gates their 365d info threshold.\n\n" +
			"ALSO gates the GitHub write-time lane: the secret-age probe authenticated, every\n" +
			"ghSecretTargets credential expected present IS present, and the one expected\n" +
			"absent (OPENBAO_ROOT_TOKEN) is absent. That feed has no age when it breaks, so\n" +
			"the age assertion above cannot reach it. Skipped with a loud message where the\n" +
			"inventory writer has never run; --require-inventory makes it a failure.\n\n" +
			"Does NOT force a rotation. assert-broad-pat-rotation already exercises one full\n" +
			"cycle and is safe only because its PAT family is throwaway; forcing lke-admin,\n" +
			"obj-key, db-admin or the state passphrase mid-run would break the cluster the\n" +
			"gate is measuring. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertRotationHealth(prom, namespace, strict, requireInventory,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().BoolVar(&requireInventory, "require-inventory", false,
		"fail if the token-inventory ConfigMap has never been written (default: report the skip). "+
			"For callers that run `llz ci token-inventory` themselves — there, an absent inventory is a break, not a fresh cluster")
	c.Flags().StringVar(&prom, "prom", "monitoring/prometheus-operated:9090",
		"the Prometheus Service as <namespace>/<name>:<port> to port-forward to")
	c.Flags().StringVar(&namespace, "namespace", "llz-reconciler", "namespace label the gauges carry")
	c.Flags().BoolVar(&strict, "strict", false,
		"also gate the non-alertable classes against the 365d info threshold")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// credVerdict is one credential's rotation-health outcome.
type credVerdict struct {
	Cred     string
	Class    string
	Age      float64 // days (valid only on the age lane, and only when Present)
	Present  bool
	Gated    bool // its class makes it eligible to fail the gate
	Optional bool // the path is opt-in; absence is not a finding
	FailWhy  string
	// Lane distinguishes the two questions this gate now asks. Empty (the
	// zero value) is the age lane, so every existing construction keeps its
	// meaning; presenceLane verdicts carry no Age and must not be printed as
	// though they did — "0 days old, SLA 90" on a credential whose problem is
	// that it does not exist reads as a passing measurement.
	Lane string
}

const presenceLane = "presence"

// expectedRotationCreds returns the credentials this gate demands a series for,
// derived from the SAME reconcilelanes.CredPaths table the sampler walks.
//
// Derived from the declaration, not from the metrics: asking Prometheus which
// credentials exist and then checking those exist is a tautology, and it would
// pass green on precisely the missing-series bug this gate is for.
func expectedRotationCreds() []expectedCred {
	out := make([]expectedCred, 0, len(reconcilelanes.CredPaths))
	for _, cp := range reconcilelanes.CredPaths {
		out = append(out, expectedCred{cp.Cred, cp.Class, cp.Optional})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cred < out[j].Cred })
	return out
}

// expectedCred is one credential this gate reasons about: what class it carries,
// and whether it is expected to EXIST at all on a given deployment.
type expectedCred struct {
	Cred     string
	Class    string
	Optional bool
}

// slaForClass returns the rotation SLA in days for a class.
func slaForClass(class string) float64 {
	if alertableCredClasses[class] {
		return rotationSLAAlertableDays
	}
	return rotationSLAInfoDays
}

// evalRotationHealth judges every expected credential against the sampled ages.
// Pure.
//
// ages maps the `cred` label to its llz_credential_age_days value. A credential
// absent from it published no series.
func evalRotationHealth(expected []expectedCred, ages map[string]float64, strict bool) []credVerdict {
	out := make([]credVerdict, 0, len(expected))
	for _, e := range expected {
		v := credVerdict{Cred: e.Cred, Class: e.Class, Optional: e.Optional,
			Gated: strict || alertableCredClasses[e.Class]}
		age, ok := ages[e.Cred]
		v.Present, v.Age = ok, age
		sla := slaForClass(e.Class)

		switch {
		// An OPT-IN path that is simply not seeded here. Its age still gates when
		// it IS present — the SLA is real — but demanding the series would red
		// every stock cluster for a credential that is correctly absent, which is
		// the permanent red this gate's own doc comment refuses for `static`.
		case !ok && e.Optional:
			v.FailWhy = ""
		case !ok && alertableCredClasses[e.Class]:
			v.FailWhy = "no llz_credential_age_days series — this credential is DECLARED in reconcilelanes.CredPaths but the " +
				"openbao-gauges lane is publishing nothing for it. It is invisible on the single pane, and " +
				"LLZCredentialRotationOverdue can never fire for it: a rule over an absent series never evaluates. " +
				"Usual cause is a missing secret/metadata read in policyReconcilerRead (a 403 fails the whole " +
				"sampler pass), or the path was never seeded"
		case !ok:
			// Unseeded optional paths (e.g. the Slack webhook when no receiver is
			// configured) legitimately 404 and are skipped by the sampler.
			v.FailWhy = ""
		case age > sla && v.Gated:
			v.FailWhy = fmt.Sprintf("%.0f days since last rotation, past its %.0f-day SLA (class %s). "+
				"%s", age, sla, e.Class, rotationRemedy(e.Class))
		}
		out = append(out, v)
	}
	return out
}

// rotationRemedy names what to do, which differs by class — the alert
// description branches on exactly this distinction and so should the gate.
func rotationRemedy(class string) string {
	switch class {
	case reconcilelanes.CredClassAutomated:
		return "A breach here means the ROTATOR is broken, not that nobody ran it — check the rotator's CronJob/lane"
	case reconcilelanes.CredClassOnDemand:
		return "A breach here means nobody has TRIGGERED it — dispatch secret-rotation.yml for this credential"
	default:
		return "Nothing automated lowers this age; it is re-seeded by hand"
	}
}

// failedCreds returns the credentials that failed.
func failedCreds(vs []credVerdict) []string {
	var out []string
	for _, v := range vs {
		if v.FailWhy != "" {
			out = append(out, v.Cred)
		}
	}
	sort.Strings(out)
	return out
}

// probeRotationHealth reads the credential-age vector.
func probeRotationHealth(prom, namespace string, strict bool) ([]credVerdict, error) {
	q := fmt.Sprintf(`llz_credential_age_days{namespace=%q}`, namespace)
	var ages map[string]float64
	err := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
		raw, gerr := get("/api/v1/query?query=" + url.QueryEscape(q))
		if gerr != nil {
			return gerr
		}
		var perr error
		ages, perr = promVectorByLabel(raw, "cred")
		return perr
	})
	if err != nil {
		return nil, err
	}
	return evalRotationHealth(expectedRotationCreds(), ages, strict), nil
}

// probeBothLanes reads the age lane and the presence lane in one pass, so the
// settle loop retries them together. A cluster that has just come up is often
// mid-way through both, and retrying one while the other is already stale would
// report a state that never existed.
func probeBothLanes(prom, namespace string, strict, require bool) ([]credVerdict, error) {
	ages, err := probeRotationHealth(prom, namespace, strict)
	if err != nil {
		return nil, err
	}
	presence, err := probePresenceHealth(prom, namespace, require)
	if err != nil {
		return nil, err
	}
	return append(ages, presence...), nil
}

func runCIAssertRotationHealth(prom, namespace string, strict, require bool, settle, interval time.Duration) error {
	fmt.Println("## Credential rotation-health assertion")

	var last []credVerdict
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		vs, err := probeBothLanes(prom, namespace, strict, require)
		last, lastErr = vs, err
		if err == nil && len(failedCreds(vs)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if err != nil {
			fmt.Printf("attempt %d: could not reach Prometheus at %s (%v) — retrying in %s\n", attempt, prom, err, interval)
		} else {
			fmt.Printf("attempt %d: %v not healthy yet — retrying in %s\n", attempt, failedCreds(vs), interval)
		}
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Fprintf(os.Stderr, "::error::could not reach Prometheus at %s within %s (%v)\n", prom, settle, lastErr)
		return fmt.Errorf("could not reach Prometheus at %s within %s: %w", prom, settle, lastErr)
	}

	for _, v := range last {
		switch {
		case v.FailWhy != "":
			fmt.Printf("FAIL: %s (%s) — %s\n", v.Cred, v.Class, v.FailWhy)
		case v.Lane == presenceLane && v.Class == "funnel" && !v.Present:
			fmt.Printf("skip: %s — the inventory writer has not run on this cluster, so the GitHub "+
				"write-time lane is unmeasured. Pass --require-inventory where the writer DOES run.\n", v.Cred)
		case v.Lane == presenceLane:
			fmt.Printf("OK: %s (%s) — present as expected\n", v.Cred, v.Class)
		case !v.Present && v.Optional:
			fmt.Printf("skip: %s (%s, opt-in) — no series; this path is not seeded on this cluster, "+
				"which is the normal state for it. Its %.0f-day SLA still gates once it is.\n",
				v.Cred, v.Class, slaForClass(v.Class))
		case !v.Present:
			fmt.Printf("skip: %s (%s) — no series; path not seeded on this cluster\n", v.Cred, v.Class)
		default:
			fmt.Printf("OK: %s (%s) — %.0f days old, SLA %.0f\n", v.Cred, v.Class, v.Age, slaForClass(v.Class))
		}
	}

	if bad := failedCreds(last); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "::error::credential rotation health: %s\n", strings.Join(bad, ", "))
		return fmt.Errorf("credential rotation health: %s", strings.Join(bad, ", "))
	}
	fmt.Println("Every rotatable credential is observed and within its SLA, and every GitHub-held one is configured as expected.")
	return nil
}

// ── the presence lane ────────────────────────────────────────────────────────
//
// Everything above gates AGE, over the credentials reconcilelanes.CredPaths declares in OpenBao.
// It has nothing to say about the OTHER feed — the GitHub write-time lane — and
// the failure mode there is not "old", it is "not there".
//
// The three ways that lane fails, none of which the age gate can see:
//
//   the probe never authenticated   newSecretAgeWriter needs a token and a repo;
//                                   the job supplied only the token, so no
//                                   write-time series existed at all. It fails
//                                   soft by design, and the soft failure was a
//                                   ::warning:: in a scheduled job's log.
//   a credential is not configured  it has no age because it has no value, so
//                                   the age gate above cannot reach it — a check
//                                   over an absent series never evaluates.
//   a root token is parked          OPENBAO_ROOT_TOKEN is supposed to be ABSENT;
//                                   present means a break-glass generate/rotate
//                                   ran and its revoke half did not, leaving a
//                                   live full-admin credential in an Actions
//                                   secret.
//
// SKIPPED WHEN THE WRITER HAS NOT RUN. On a freshly bootstrapped cluster the
// inventory ConfigMap does not exist yet — the writer is a scheduled job — and
// failing there would gate the e2e suite on a job that legitimately has not run.
// The skip is reported loudly rather than silently, and --require-inventory turns
// it into a failure for the one caller that runs the writer moments earlier.

// evalPresenceHealth judges the write-time lane. Pure.
//
// configured maps `cred` → llz_credential_configured. probeOK is the
// llz_credential_secret_probe_ok sample; probeSeen distinguishes "the writer said
// the probe failed" from "the writer has never run here", which need different
// answers and would otherwise be the same absent series.
func evalPresenceHealth(configured map[string]float64, probeOK float64, probeSeen, require bool) []credVerdict {
	if !probeSeen {
		v := credVerdict{Cred: "token-inventory", Class: "funnel", Lane: presenceLane, Gated: require}
		v.FailWhy = "no llz_credential_secret_probe_ok series — `llz ci token-inventory` has not written " +
			"the inventory ConfigMap on this cluster, so the whole GitHub write-time lane is unmeasured. " +
			"Expected on a freshly bootstrapped cluster (the writer is a scheduled job); a FAILURE " +
			"anywhere the writer runs, which is what --require-inventory asserts"
		if !require {
			v.FailWhy = ""
			v.Present = false
		}
		return []credVerdict{v}
	}

	out := []credVerdict{{
		Cred: "token-inventory", Class: "funnel", Lane: presenceLane, Present: true, Gated: true,
		FailWhy: func() string {
			if probeOK == 1 {
				return ""
			}
			return "llz_credential_secret_probe_ok = 0 — the writer could not build its GitHub " +
				"secrets-metadata client, so no credential write time was measured. Check that the job " +
				"running token-inventory exports GH_REPO and a token with Secrets access"
		}(),
	}}

	for _, tgt := range ghSecretTargets {
		cred := credLabelForSecret(tgt.name)
		v := credVerdict{Cred: cred, Class: tgt.class, Lane: presenceLane, Gated: true}
		got, ok := configured[cred]
		v.Present = ok
		switch {
		case tgt.expect == credExpectOptional:
			// Legitimately absent on a healthy deployment (the Harbor robot pair on
			// a standby peer, before the ACTIVE peer's provisioner has published
			// them). Visible on the dashboard, never a gate in either direction.
			v.Gated = false
		case !ok:
			v.FailWhy = "no llz_credential_configured series, although the probe reported OK. Either " +
				"the writer could not READ this credential (a 403 on the environment scope is the " +
				"likely one — environment-secret metadata needs different token permissions from " +
				"repo-scoped, and the OpenBao credentials are environment-scoped), or the funnel " +
				"between writer and reconciler is broken. It is NOT evidence the credential is missing"
		case tgt.expect == credExpectPresent && got != 1:
			v.FailWhy = "expected present and the GitHub secrets API reports it ABSENT. It has no age " +
				"because it has no value, so no age rule can fire for it. Seed it (docs/secrets.md), or " +
				"drop it from ghSecretTargets if this instance genuinely does not use it"
		case tgt.expect == credExpectAbsent && got != 0:
			v.FailWhy = "expected ABSENT and it is set. A root token is ephemeral by design — bootstrap " +
				"revokes it and the recovery quorum is what survives — so this is a live full-admin " +
				"credential left by a break-glass whose revoke never ran. Dispatch " +
				"llz-breakglass-openbao.yml with action=revoke"
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cred < out[j].Cred })
	return out
}

// probePresenceHealth reads the two write-time-lane vectors.
func probePresenceHealth(prom, namespace string, require bool) ([]credVerdict, error) {
	var configured map[string]float64
	var probeOK float64
	var probeSeen bool
	var perr error
	err := withPrometheus(prom, func(get func(string) ([]byte, error)) error {
		raw, gerr := get("/api/v1/query?query=" +
			url.QueryEscape(fmt.Sprintf(`llz_credential_configured{namespace=%q}`, namespace)))
		if gerr != nil {
			return gerr
		}
		if configured, perr = promVectorByLabel(raw, "cred"); perr != nil {
			return perr
		}
		raw, gerr = get("/api/v1/query?query=" +
			url.QueryEscape(fmt.Sprintf(`llz_credential_secret_probe_ok{namespace=%q}`, namespace)))
		if gerr != nil {
			return gerr
		}
		// NOT promVectorByLabel: this series carries no `cred` label, and that
		// helper skips any sample whose label is empty (`if key == "" continue`).
		// Reading it through there returned an empty map every time, so probeSeen
		// was permanently false — the presence lane silently checked nothing, and
		// --require-inventory would have failed EVERY run on every cluster while
		// reporting "the writer has not run here". The unit tests missed it
		// because they exercise evalPresenceHealth directly and never crossed this
		// parsing seam.
		probeOK, probeSeen, perr = promFirstSample(raw)
		return perr
	})
	if err != nil {
		return nil, err
	}
	return evalPresenceHealth(configured, probeOK, probeSeen, require), nil
}

// promFirstSample reads the value of the first sample in an instant-query
// response, regardless of its labels, and reports whether there was one.
//
// It exists because promVectorByLabel — the helper every other gauge gate uses —
// indexes samples BY a label and drops any whose value for it is empty. That is
// right for the per-credential series and silently wrong for an aggregate:
// `llz_credential_secret_probe_ok` has no `cred` label, so every sample was
// discarded and the caller could not tell an unreachable funnel from one that had
// never run. A query error is still an error; an empty result is a legitimate
// "no such series yet" and returns ok=false.
func promFirstSample(raw []byte) (float64, bool, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, false, fmt.Errorf("unparseable Prometheus response: %w", err)
	}
	if resp.Status != "success" {
		detail := resp.Error
		if detail == "" {
			detail = "status=" + resp.Status
		}
		return 0, false, fmt.Errorf("prometheus returned an error: %s", detail)
	}
	for _, r := range resp.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		str, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(str, 64)
		if err != nil {
			continue
		}
		return f, true, nil
	}
	return 0, false, nil
}
