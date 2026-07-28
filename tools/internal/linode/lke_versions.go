package linode

// lke_versions.go — the LKE-Enterprise version catalog, so `llz doctor` can ask
// the ACCOUNT whether the k8sVersion an instance pins is actually offered to it,
// instead of finding out inside a terraform apply.
//
// Route: /v4beta/lke/tiers/{tier}/versions. LKE-Enterprise is a v4beta,
// enterprise-tier-only product (see docs/adr/0005-managed-app-platform.md), so
// the standard /v4/lke/versions catalog is the wrong list to check against — it
// answers for a product this landing zone does not use.
//
// The route's existence is verified (an unknown Linode path returns 404 before
// auth; this one returns 401), but the response BODY cannot be inspected without
// an entitled token. So callers must treat every unexpected shape as UNKNOWN and
// stay quiet, never as "not offered" — see doctor_linode.go, which is written to
// that contract.

import (
	"context"
	"strings"
)

// LKETierEnterprise is the tier segment for LKE-Enterprise.
const LKETierEnterprise = "enterprise"

// ListLKEVersions returns the Kubernetes versions the ACCOUNT may create in the
// given LKE tier, newest-first as the API returns them.
//
// Each entry is reported under `id` (the Linode list convention). An entry whose
// id is missing or non-string is skipped rather than guessed at: a partial list
// is still useful for reporting, and the caller never treats a short list as
// proof of absence.
func (c *Client) ListLKEVersions(ctx context.Context, tier string) ([]string, error) {
	raw, err := c.listAllPages(ctx, "/v4beta/lke/tiers/"+tier+"/versions")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range raw {
		if id, ok := m["id"].(string); ok && strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out, nil
}
