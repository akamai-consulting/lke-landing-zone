package main

// reconcile_tokens.go is the READER half of the credential single-pane-of-glass
// (writer: ci_token_inventory.go). External CI tokens can only be measured by a job
// that holds them, so the scheduled token-inventory job writes their expiry into the
// llz-token-inventory ConfigMap; this sampler reads it every pass and re-exposes it
// as Prometheus gauges, so the cluster is the single source of truth and PromQL can
// alert BEFORE a token expires. Read-only (a ConfigMap GET), so it is never leader-gated.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// sampleTokenInventory reads the llz-token-inventory ConfigMap from the reconciler's
// own namespace and publishes one llz_token_expiry_timestamp_seconds +
// llz_token_audit_ok series per token, plus an inventory heartbeat. A 404 (the
// scheduled writer hasn't run yet) is not an error — the sampler no-ops until the
// funnel is primed, and LLZTokenInventoryStale covers a funnel that later stalls.
func sampleTokenInventory(ctx context.Context, client nodeGetter, reg *metrics.Registry, now time.Time) error {
	ns := podNamespace()
	obj, status, err := client.GetJSON(ctx, "/api/v1/namespaces/"+ns+"/configmaps/llz-token-inventory")
	if err != nil {
		return err
	}
	if status == 404 {
		return nil // not written yet — nothing to publish
	}
	if status < 200 || status >= 300 || obj == nil {
		return fmt.Errorf("GET llz-token-inventory ConfigMap: status %d", status)
	}

	raw := configMapData(obj, "inventory.json")
	if raw == "" {
		return nil // present but empty — treat as not-yet-primed
	}
	var inv tokenInventory
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		return fmt.Errorf("parse llz-token-inventory: %w", err)
	}

	reg.SetGauge("llz_token_inventory_updated_timestamp_seconds",
		"unix time the token inventory was last written by the CI token-inventory job",
		nil, float64(inv.Updated))
	reg.SetGauge("llz_token_inventory_tokens", "count of tokens in the inventory", nil, float64(len(inv.Tokens)))

	for _, t := range inv.Tokens {
		labels := map[string]string{"provider": t.Provider, "token": t.Name}
		if t.Expiry > 0 {
			reg.SetGauge("llz_token_expiry_timestamp_seconds",
				"unix time the CI token expires (0/absent when it never expires or is unknown)",
				labels, float64(t.Expiry))
		}
		auditOK := 1.0
		if t.State == tokenStateBreach {
			auditOK = 0
		}
		reg.SetGauge("llz_token_audit_ok",
			"1 if the CI token satisfies the expiry policy, 0 on a breach (no-expiry / expired / over-policy)",
			labels, auditOK)
	}

	// Write-time ages for the credentials that have no expiry to read and cannot
	// live in OpenBao (ADR 0009). Deliberately published as llz_credential_age_days
	// — the SAME metric the OpenBao sampler uses — because it means exactly the
	// same thing ("days since this credential was last written") and only the
	// SOURCE differs. Reusing it means the existing dashboard panels and both
	// alert rules pick these up with no query changes.

	// Did the write-time probe run at all? An empty Secrets list cannot answer
	// that — it reads identically whether every credential was measured and none
	// exist, or the probe never authenticated. The writer says so explicitly now,
	// because "never authenticated" is what was actually happening: the
	// credential-single-pane job passed a token but no GH_REPO, so the probe
	// errored on every run and the whole write-time half of the single pane was
	// dark behind a ::warning:: in a scheduled job's log.
	//
	// An inventory with no verdict at all is from a writer that predates this
	// field; publish nothing rather than guess, so an instance mid-upgrade does
	// not page for a probe that may be working fine.
	switch inv.SecretProbe {
	case secretProbeOK:
		reg.SetGauge("llz_credential_secret_probe_ok",
			"1 if the GitHub secrets-metadata probe (credential write-time ages) could run, 0 if it could not authenticate",
			nil, 1)
	case secretProbeUnavailable:
		reg.SetGauge("llz_credential_secret_probe_ok",
			"1 if the GitHub secrets-metadata probe (credential write-time ages) could run, 0 if it could not authenticate",
			nil, 0)
	}

	// Accumulated and published as WHOLE FAMILIES below, not per-credential.
	// SetGauge has no delete, so a credential that leaves ghSecretTargets leaves
	// its last sample frozen — and "drop it from ghSecretTargets" is the remedy
	// this very ruleset recommends for a credential an instance does not use, so
	// taking that advice would have made the alert permanent until someone
	// restarted the reconciler. These two families have exactly one writer (this
	// function), which is what makes a whole-family replace safe here and unsafe
	// for llz_credential_age_days, whose samples the OpenBao sampler also writes.
	var configured, presenceOK []metrics.GaugeSample
	for _, sec := range inv.Secrets {
		cred := credLabelForSecret(sec.Name)

		// PRESENCE, published unconditionally — including for a secret that is not
		// configured at all. That case used to `continue` before publishing
		// anything, which quietly reintroduced the exact blind spot ADR 0009
		// claimed the GitHub API had closed: it argued no companion absence alert
		// was needed because "the API distinguishes them", and it does — the
		// distinction was carried all the way here as State/UpdatedAt and then
		// dropped on the floor. A never-configured TF_STATE_ENCRYPTION_PASSPHRASE
		// or a missing recovery key published NO series, and a rule over an absent
		// series never evaluates, so it looked identical to a healthy one on both
		// the dashboard and every alert.
		//
		// `expect` is a label rather than a filter because presence is not
		// uniformly good: OPENBAO_ROOT_TOKEN is supposed to be ABSENT (bootstrap
		// revokes it), so for that one a 1 here is the finding. Two rules read
		// this series in opposite directions — LLZCredentialUnconfigured and
		// LLZCredentialRootTokenParked — and both need the label to say which way.
		expect := sec.Expect
		if expect == "" {
			expect = credExpectPresent // inventory written by an older llz
		}
		// Publish presence ONLY when the writer actually learned it. `ok` and
		// `absent` are both answers; `unknown` means the API refused (a 403 on the
		// environment scope, a 5xx) and we know nothing.
		//
		// Publishing 0 for `unknown` would be the same conflation this series
		// exists to remove: LLZCredentialUnconfigured reads 0 as "seed this
		// credential", so a token-permission problem would page as a missing
		// credential and send the operator to the wrong runbook. Silence here is
		// not a blind spot — secretProbeVerdict flips the funnel gauge to 0 for
		// exactly this case, so LLZCredentialSecretProbeUnavailable fires instead
		// and names the real fault.
		//
		// An inventory from a writer predating tokenStateAbsent reports absent
		// credentials as `unknown` and so publishes nothing for them. That is the
		// pre-existing behaviour, and degrading to it during an upgrade is the
		// safe direction — no series rather than a false one.
		switch sec.State {
		case tokenStateOK, tokenStateAbsent:
			present := sec.State == tokenStateOK
			// LABELS ARE `cred` ONLY, and that is a correctness requirement rather
			// than tidiness. tools/internal/metrics upserts keyed by the RENDERED
			// LABEL SET and has no delete: SetGauge with a different label value
			// adds a series, it does not replace one, and the old sample is served
			// at its last value for the life of the pod.
			//
			// `class` and `expect` are CLASSIFICATIONS — they change when the
			// writer's llz is upgraded, independently of this long-lived pod. The
			// first shape of this metric carried both, so the HARBOR_* move from
			// present to optional (made two commits ago, in this same branch)
			// would have left a stale {expect="present"} 0 behind, firing
			// LLZCredentialUnconfigured forever until someone restarted the
			// reconciler. That is precisely the permanent noise the class label was
			// introduced to avoid one metric over.
			//
			// So the classification is applied HERE and only the verdict is
			// published. A reclassification now flips a value on a series that
			// already exists; it can never mint a second one.
			configured = append(configured, metrics.GaugeSample{
				Labels: map[string]string{"cred": cred}, Value: boolGauge(present)})
			presenceOK = append(presenceOK, metrics.GaugeSample{
				Labels: map[string]string{"cred": cred},
				Value:  boolGauge(presenceMatchesExpectation(expect, present))})
		}

		if sec.UpdatedAt == "" {
			continue // absent or unreadable — either way there is no age to publish
		}
		t, err := time.Parse(time.RFC3339, sec.UpdatedAt)
		if err != nil {
			continue
		}
		// SetGauge, not the family replace: the OpenBao sampler writes this same
		// metric for its own credentials, and replacing the family here would
		// delete every one of them on each pass.
		reg.SetGauge("llz_credential_age_days",
			"days since the credential was last written (class: automated|on-demand|generate-once|tracks-source|static)",
			map[string]string{"cred": cred, "class": sec.Class},
			float64(health.DaysSince(t, now)))
	}

	// Published even when empty — that is the point. An empty inventory (the probe
	// could not authenticate, or every credential was retired) must REMOVE the
	// series rather than leave the previous pass's verdicts standing.
	reg.SetGaugeFamily("llz_credential_configured",
		"1 if the credential is configured as a GitHub Actions secret, 0 if the API reports it absent",
		configured)
	reg.SetGaugeFamily("llz_credential_presence_ok",
		"1 if the credential's presence matches what is expected of it (see ghSecretTargets: present|optional|absent)",
		presenceOK)
	return nil
}

// credLabelForSecret turns a GitHub secret name into the lower-kebab `cred`
// label the credential dashboard uses, so these sit alongside the OpenBao-sourced
// series instead of shouting in SCREAMING_SNAKE next to them.
func credLabelForSecret(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// configMapData returns the string value of data[key] from a ConfigMap object,
// or "" when absent/mistyped (defensive against a hand-edited or empty ConfigMap).
func configMapData(obj map[string]any, key string) string {
	data, ok := obj["data"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := data[key].(string)
	return s
}

// presenceMatchesExpectation applies the ghSecretTargets classification, so the
// published series carries a verdict under an immutable label set rather than the
// raw fact plus a label that says how to read it.
//
// `optional` is always satisfied: the Harbor robot pair is published by the
// ACTIVE peer's provisioner, so a standby legitimately has neither until it has
// run, and neither presence nor absence is a finding there.
func presenceMatchesExpectation(expect string, present bool) bool {
	switch expect {
	case credExpectAbsent:
		return !present
	case credExpectOptional:
		return true
	default: // credExpectPresent, and an empty value from an older writer
		return present
	}
}
