package health

import (
	"encoding/json"
	"fmt"
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

// NOTE: the control-plane ACL outcome check (ControlPlaneACLState /
// ClassifyControlPlaneACL) lived here until its only caller was removed by
// f3b3bcb, which extracted the EAA/internal-CIDR firewall feature to a private
// repo. It sat orphaned afterwards — unreachable from cmd/llz and unusable from
// outside the module (internal/). Removed rather than left as extraction
// residue; recover from history if the feature returns.
