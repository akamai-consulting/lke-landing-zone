package main

// import_aplvalues.go parses the APL/Otomi "DOWNLOAD PLATFORM VALUES" file — the
// merged, resolved platform configuration — into aplSignals. It is the preferred
// APL source for `llz import scan` (--apl-values), replacing the repo clone:
// authoritative APL version, domain suffix, enabled apps, teams, and object-store
// buckets in one file.
//
// SECURITY: that download also contains DECRYPTED secrets (the SOPS age private
// key, object-store keys, the admin password, …). parseAplValues decodes into a
// strict ALLOWLIST struct — only the config keys below — so secret values can
// never reach the report. We never bind kms/otomi.adminPassword/obj.*.accessKey.

import (
	"encoding/json"

	"sigs.k8s.io/yaml"
)

// parseAplValues reads the merged platform-values YAML into aplSignals. Only the
// allowlisted config keys are bound; everything else (including every secret) is
// ignored by construction.
func parseAplValues(content string) (aplSignals, error) {
	var v struct {
		Cluster struct {
			DomainSuffix string `json:"domainSuffix"`
		} `json:"cluster"`
		Apps map[string]struct {
			Enabled *bool `json:"enabled"`
		} `json:"apps"`
		TeamConfig map[string]struct{} `json:"teamConfig"`
		Otomi      struct {
			Version        string `json:"version"`
			HasExternalDNS *bool  `json:"hasExternalDNS"`
			HasExternalIDP *bool  `json:"hasExternalIDP"`
			IsMultitenant  *bool  `json:"isMultitenant"`
		} `json:"otomi"`
		Obj struct {
			// provider mixes a per-type object ("linode": {...}) with a sibling
			// "type": "<string>" key, so decode each value lazily and skip the ones
			// that aren't provider objects. We bind ONLY bucket names + region.
			Provider map[string]json.RawMessage `json:"provider"`
		} `json:"obj"`
	}
	if err := yaml.Unmarshal([]byte(content), &v); err != nil {
		return aplSignals{}, err
	}

	var sig aplSignals
	for name := range v.TeamConfig {
		if name != "" && name != "admin" {
			sig.Teams = append(sig.Teams, name)
		}
	}
	for name, app := range v.Apps {
		if app.Enabled == nil {
			continue
		}
		if *app.Enabled {
			sig.EnabledApps = append(sig.EnabledApps, name)
		} else {
			sig.DisabledApps = append(sig.DisabledApps, name)
		}
	}
	sig.Teams = dedupeSorted(sig.Teams)
	sig.EnabledApps = dedupeSorted(sig.EnabledApps)
	sig.DisabledApps = dedupeSorted(sig.DisabledApps)

	sig.DomainSuffix = v.Cluster.DomainSuffix
	if sig.DomainSuffix != "" {
		sig.Domains = []string{sig.DomainSuffix}
	}
	sig.AplVersion = v.Otomi.Version
	sig.ExternalDNS = v.Otomi.HasExternalDNS
	sig.ExternalIDP = v.Otomi.HasExternalIDP
	sig.Multitenant = v.Otomi.IsMultitenant

	// Take the provider that actually carries buckets (the configured backend);
	// the sibling "type" string and any non-object value simply fail to decode and
	// are skipped.
	for _, raw := range v.Obj.Provider {
		var p struct {
			Region  string            `json:"region"`
			Buckets map[string]string `json:"buckets"`
		}
		if json.Unmarshal(raw, &p) != nil || len(p.Buckets) == 0 {
			continue
		}
		sig.ObjectRegion = p.Region
		sig.ObjectBuckets = p.Buckets
		break
	}
	return sig, nil
}

// firstAplSignals returns the APL signals from the first apl-role inventory, or
// nil. Used by buildReport to fold authoritative APL facts (version, domain
// suffix, enabled apps) into the cluster-derived report.
func firstAplSignals(repos []repoInventory) *aplSignals {
	for _, r := range repos {
		if r.Role == "apl" && r.APL != nil {
			return r.APL
		}
	}
	return nil
}

// aplAppComponent maps an APL app key to the LLZ component toggle it enables.
var aplAppComponent = map[string]string{
	"harbor":           "harbor",
	"loki":             "observability",
	"grafana":          "observability",
	"prometheus":       "observability",
	"alertmanager":     "observability",
	"otel":             "observability",
	"kyverno":          "policyEngine",
	"trivy":            "imageScanning",
	"gitea":            "gitea",
	"cert-manager":     "certManager",
	"external-secrets": "externalSecrets",
	"argocd":           "argocd",
}

// aplComponentsFromApps maps APL's enabled apps to LLZ component toggles — the
// authoritative "what's on" signal (vs. inferring from running namespaces).
func aplComponentsFromApps(enabled []string) map[string]bool {
	out := map[string]bool{}
	for _, app := range enabled {
		if c := aplAppComponent[app]; c != "" {
			out[c] = true
		}
	}
	return out
}
