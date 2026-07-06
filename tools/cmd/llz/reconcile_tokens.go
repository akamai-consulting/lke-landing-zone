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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// sampleTokenInventory reads the llz-token-inventory ConfigMap from the reconciler's
// own namespace and publishes one llz_token_expiry_timestamp_seconds +
// llz_token_audit_ok series per token, plus an inventory heartbeat. A 404 (the
// scheduled writer hasn't run yet) is not an error — the sampler no-ops until the
// funnel is primed, and LLZTokenInventoryStale covers a funnel that later stalls.
func sampleTokenInventory(ctx context.Context, client nodeGetter, reg *metrics.Registry) error {
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
	return nil
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
