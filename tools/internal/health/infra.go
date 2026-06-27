package health

import (
	"encoding/json"
	"fmt"
	"strings"
)

// infra.go ports the OpenBao seal/HA (1a), admission-webhook (1d) / Service
// endpoint readiness, and cloud-firewall bootstrap (the kube-system Secret +
// ConfigMap) checks.

// ── OpenBao seal / Raft HA (1a) ──────────────────────────────────────────────

// BaoStatus is the parsed `bao status -format=json` state we classify on.
type BaoStatus struct {
	Initialized bool
	Sealed      bool
	HAMode      string // active | standby | standalone
}

// ParseBaoStatus parses `bao status -format=json`, preserving literal booleans
// and defaulting ONLY on null — the script's hard-won fix for jq's `//` treating
// a literal false as missing: initialized defaults false, sealed defaults true
// (fail-safe), and HA mode is derived from is_self / ha_enabled.
func ParseBaoStatus(raw []byte) (BaoStatus, error) {
	var j struct {
		Initialized *bool `json:"initialized"`
		Sealed      *bool `json:"sealed"`
		IsSelf      *bool `json:"is_self"`
		HAEnabled   *bool `json:"ha_enabled"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return BaoStatus{}, err
	}
	s := BaoStatus{
		Initialized: j.Initialized != nil && *j.Initialized,
		Sealed:      j.Sealed == nil || *j.Sealed,
		HAMode:      "standalone",
	}
	switch {
	case j.IsSelf != nil && *j.IsSelf:
		s.HAMode = "active"
	case j.HAEnabled != nil && *j.HAEnabled:
		s.HAMode = "standby"
	}
	return s, nil
}

// ClassifyBaoSeal classifies one OpenBao pod's seal state: initialized + unsealed
// passes; not-initialized (not part of the Raft cluster) and sealed both fail.
func ClassifyBaoSeal(s BaoStatus) (Category, string) {
	switch {
	case s.Initialized && !s.Sealed:
		return CatOK, "initialized, unsealed, HA Mode=" + s.HAMode
	case !s.Initialized:
		return CatFail, "NOT initialized — not part of the Raft cluster; needs raft join + unseal"
	default:
		return CatFail, "sealed — pods auto-unseal from the static seal key at boot; check the openbao-unseal-key Secret and Raft storage"
	}
}

// ClassifyLeaderCount classifies the cluster-wide active-leader count: zero
// active leaders (nobody serving writes) and more than one (Raft split-brain —
// writes diverge) are both failures.
func ClassifyLeaderCount(replicas, activeCount int) (Category, string) {
	switch {
	case replicas > 0 && activeCount == 0:
		return CatFail, "OpenBao has no active leader (HA Mode=active not observed on any pod)"
	case activeCount > 1:
		return CatFail, fmt.Sprintf("OpenBao has %d active leaders (split-brain — Raft writes will diverge)", activeCount)
	default:
		return CatOK, "exactly one active OpenBao leader"
	}
}

// ── Endpoint readiness (shared by webhooks 1d and Service endpoints 3e) ──────

// EndpointSlice is the subset of an EndpointSlice we read.
type EndpointSlice struct {
	Endpoints []struct {
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
	} `json:"endpoints"`
}

// CountReadyEndpoints counts endpoints across slices whose conditions.ready is
// true — treating an absent ready field as true (jq `.conditions.ready // true`).
func CountReadyEndpoints(slices []EndpointSlice) int {
	n := 0
	for _, s := range slices {
		for _, e := range s.Endpoints {
			if e.Conditions.Ready == nil || *e.Conditions.Ready {
				n++
			}
		}
	}
	return n
}

// ClassifyWebhookBackend classifies an admission webhook's backing Service:
// a missing Service or zero ready endpoints fails; otherwise it passes.
func ClassifyWebhookBackend(serviceExists bool, readyCount int) (Category, string) {
	switch {
	case !serviceExists:
		return CatFail, "backing Service MISSING (configured but not present)"
	case readyCount == 0:
		return CatFail, "0 ready endpoints (controller down / NP blocking / selector drift)"
	default:
		return CatOK, fmt.Sprintf("%d ready endpoint(s)", readyCount)
	}
}

// ── Cloud-firewall bootstrap (kube-system Secret + ConfigMap) ────────────────

// ClassifyFirewallToken classifies the kube-system/linode Secret token: present
// + non-empty passes; present-but-empty and absent both fail.
func ClassifyFirewallToken(secretExists bool, token string) (Category, string) {
	switch {
	case !secretExists:
		return CatFail, "Secret kube-system/linode missing — bootstrap step did not seed it"
	case token == "":
		return CatFail, "Secret kube-system/linode exists but token key is empty"
	default:
		return CatOK, "Secret kube-system/linode has non-empty token"
	}
}

// ClassifyFirewallConfigKey classifies a linode-internal-cidr-firewall-config
// ConfigMap key: a set value passes; an empty LKE_CLUSTER_ID is only
// informational (control-plane ACL reconciliation disabled); any other empty key
// (LINODE_FIREWALL_ID + the manifest-owned keys) is operator-deferred until the
// firewall bootstrap / Argo App has run.
func ClassifyFirewallConfigKey(key, value string) Category {
	if value != "" {
		return CatOK
	}
	if key == "LKE_CLUSTER_ID" {
		return CatOK
	}
	return CatDeferred
}

// ── Control-plane ACL outcome verification ───────────────────────────────────

// ControlPlaneACLState is the live LKE control-plane ACL as the health check
// observes it: whether access is restricted (Enabled) and the allowed CIDR sets.
// A local mirror of linode.ControlPlaneACL so the health package stays decoupled
// from the Linode client (cmd/llz maps one to the other).
type ControlPlaneACLState struct {
	Enabled bool
	IPv4    []string
	IPv6    []string
}

// ClassifyControlPlaneACL verifies the live control-plane ACL actually enforces
// access and contains every EAA/bastion CIDR the firewall-controller is meant to
// keep allowed (expectedIPv4 = ParsedCIDRs.ControlPlaneACLIPv4(), expectedIPv6 =
// the EAA IPv6 set). It is the OUTCOME check the input-only firewall-bootstrap
// section can't be: a SUPERSET test where extra live entries (CI-runner leases,
// operator additions) are fine but a missing expected CIDR is a hard failure —
// the exact symptom of a controller that resolves the CIDRs yet never writes them
// (e.g. a stale RBAC Role 403ing the runner-acl ConfigMap read, which trips the
// reconcile fail-safe and silently skips the ACL PUT).
//
// A disabled ACL (open to all) fails regardless of contents. With nothing
// expected — template and committed fallback both empty — there is nothing to
// assert, so it is informational rather than a pass we can vouch for.
func ClassifyControlPlaneACL(acl ControlPlaneACLState, expectedIPv4, expectedIPv6 []string) (Category, string) {
	if !acl.Enabled {
		return CatFail, "control-plane ACL is DISABLED (open to all) — the firewall-controller should keep it enforced"
	}
	if len(expectedIPv4) == 0 && len(expectedIPv6) == 0 {
		return CatWarn, "no EAA/bastion CIDRs resolved from the firewall template — nothing to verify"
	}
	missing := append(missingHostCIDRs(acl.IPv4, expectedIPv4, "/32"), missingHostCIDRs(acl.IPv6, expectedIPv6, "/128")...)
	if len(missing) > 0 {
		return CatFail, fmt.Sprintf("control-plane ACL is missing %d of %d expected EAA/bastion CIDR(s) — "+
			"firewall-controller resolved them but did not write them (check its logs for "+
			"\"skipping control-plane ACL update\"): %s",
			len(missing), len(expectedIPv4)+len(expectedIPv6), strings.Join(truncateList(missing, 8), ", "))
	}
	return CatOK, fmt.Sprintf("control-plane ACL enforced; all %d expected EAA/bastion CIDR(s) present",
		len(expectedIPv4)+len(expectedIPv6))
}

// missingHostCIDRs returns the entries of want absent from have, treating a bare
// host address and its single-host CIDR (hostSuffix = "/32" for v4, "/128" for
// v6) as equal — the only normalization Linode applies to the ACL address set, so
// "1.2.3.4" and "1.2.3.4/32" must not read as a spurious miss.
func missingHostCIDRs(have, want []string, hostSuffix string) []string {
	set := make(map[string]bool, len(have)*2)
	for _, h := range have {
		set[h] = true
		set[strings.TrimSuffix(h, hostSuffix)] = true
	}
	var missing []string
	for _, w := range want {
		if set[w] || set[strings.TrimSuffix(w, hostSuffix)] {
			continue
		}
		missing = append(missing, w)
	}
	return missing
}

// truncateList caps a list for display, appending an "(+N more)" marker so a long
// miss list does not flood the convergence summary.
func truncateList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("(+%d more)", len(s)-n))
}
