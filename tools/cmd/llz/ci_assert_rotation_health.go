package main

// ci_assert_rotation_health.go implements `llz ci assert-rotation-health` — the
// gate on the credential-rotation lifecycle.
//
// THE COUPLING IT GUARDS. credPaths (reconcile_openbao.go) DECLARES every
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
// visibly old" (credClassStatic's own comment). A silently-missing series is the
// native failure of this subsystem, and nothing gated it.
//
// WHAT IT ASSERTS. For every credential in credPaths whose class is ALERTABLE
// (automated / on-demand — the classes something is expected to rotate):
//
//   1. a llz_credential_age_days series exists for it, and
//   2. its age is within the SLA its class carries.
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
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	credClassAutomated: true,
	credClassOnDemand:  true,
}

func ciAssertRotationHealthCmd() *cobra.Command {
	var prom, namespace string
	var settle, interval int
	var strict bool
	c := &cobra.Command{
		Use:   "assert-rotation-health",
		Short: "fail unless every rotatable credential is being observed and is within its rotation SLA",
		Long: "Gates the credential-rotation lifecycle. For every credential credPaths declares\n" +
			"with an ALERTABLE class (automated / on-demand), asserts a\n" +
			"llz_credential_age_days series exists AND its age is within the class SLA.\n\n" +
			"The missing series is the point. credPaths declares the credential, the\n" +
			"openbao-gauges lane samples it, and LLZCredentialRotationOverdue alerts on the\n" +
			"result — so a credential that is declared but publishes nothing disappears from\n" +
			"the single pane AND can never fire an alert, because a rule over an absent\n" +
			"series never evaluates. That is the native failure of this subsystem (the\n" +
			"`static` class exists because a whole group of paths had silently published no\n" +
			"series at all) and nothing gated it.\n\n" +
			"Non-alertable classes (generate-once / tracks-source / static) are reported,\n" +
			"never gated: nothing will ever lower their age, so failing on it would be a\n" +
			"permanent red. --strict also gates their 365d info threshold.\n\n" +
			"Does NOT force a rotation. assert-broad-pat-rotation already exercises one full\n" +
			"cycle and is safe only because its PAT family is throwaway; forcing lke-admin,\n" +
			"obj-key, db-admin or the state passphrase mid-run would break the cluster the\n" +
			"gate is measuring. Read-only. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertRotationHealth(prom, namespace, strict,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
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
	Cred    string
	Class   string
	Age     float64 // days (valid only when Present)
	Present bool
	Gated   bool // its class makes it eligible to fail the gate
	FailWhy string
}

// expectedRotationCreds returns the credentials this gate demands a series for,
// derived from the SAME credPaths table the sampler walks.
//
// Derived from the declaration, not from the metrics: asking Prometheus which
// credentials exist and then checking those exist is a tautology, and it would
// pass green on precisely the missing-series bug this gate is for.
func expectedRotationCreds() []struct{ Cred, Class string } {
	out := make([]struct{ Cred, Class string }, 0, len(credPaths))
	for _, cp := range credPaths {
		out = append(out, struct{ Cred, Class string }{cp.cred, cp.class})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cred < out[j].Cred })
	return out
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
func evalRotationHealth(expected []struct{ Cred, Class string }, ages map[string]float64, strict bool) []credVerdict {
	out := make([]credVerdict, 0, len(expected))
	for _, e := range expected {
		v := credVerdict{Cred: e.Cred, Class: e.Class, Gated: strict || alertableCredClasses[e.Class]}
		age, ok := ages[e.Cred]
		v.Present, v.Age = ok, age
		sla := slaForClass(e.Class)

		switch {
		case !ok && alertableCredClasses[e.Class]:
			v.FailWhy = "no llz_credential_age_days series — this credential is DECLARED in credPaths but the " +
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
	case credClassAutomated:
		return "A breach here means the ROTATOR is broken, not that nobody ran it — check the rotator's CronJob/lane"
	case credClassOnDemand:
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

func runCIAssertRotationHealth(prom, namespace string, strict bool, settle, interval time.Duration) error {
	fmt.Println("## Credential rotation-health assertion")

	var last []credVerdict
	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		vs, err := probeRotationHealth(prom, namespace, strict)
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
	fmt.Println("Every rotatable credential is observed and within its SLA.")
	return nil
}
