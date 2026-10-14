package clusterspec

// Team provisioning overlay — the managed complement to spec.teams.
//
// On self-installed apl-core LLZ rendered teamConfig into values.yaml and apl-core
// provisioned each team (namespace + Keycloak group `team-<name>` + realm role).
// The managed pivot (ADR-0005) stopped rendering values.yaml, which silently
// dropped that delivery: spec.teams then provisioned NOTHING on apl-core, so
// bao-configure skipped the team's OpenBao role and team-OIDC login had no group
// claim. This overlay restores it the apl-overlay way: `llz render` emits each
// team as apl-core's native CRs, and the apl-overlay reconciler git-syncs them
// onto apl-<env> at env/teams/<name>/, where apl-operator provisions the team.
//
// Shape validated live on a managed cluster: pushing env/teams/<name>/{settings,
// apps}.yaml made apl-operator create the namespace + the Keycloak group + realm
// role team-<name>. AplTeamSettingSet is the core declaration; AplTeamTool sizes
// the team tools. Values are secure, minimal defaults — the platform admin tunes
// them in the console; LLZ only needs the team to EXIST so login works.

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// OverlayTeamsFile is the per-env manifest listing team names (spec.teams is instance-wide, rendered per env); the
	// reconciler reads it, then fans each team's CRs out to env/teams/<name>/.
	OverlayTeamsFile = "teams.yaml"
	// TeamSettingsFile / TeamAppsFile are the per-team CR filenames apl-core reads
	// under env/teams/<name>/.
	TeamSettingsFile = "settings.yaml"
	TeamAppsFile     = "apps.yaml"
)

// teamsManifestDoc marshals/parses the instance-wide team-name manifest.
type teamsManifestDoc struct {
	Teams []string `yaml:"teams"`
}

// RenderTeamsManifest emits the teams.yaml manifest (the ordered team names the
// reconciler fans out). Empty teams → an empty list (a clean no-op downstream).
func RenderTeamsManifest(teams []Team) string {
	names := make([]string, 0, len(teams))
	for _, t := range teams {
		names = append(names, t.Name)
	}
	return marshalYAML(teamsManifestDoc{Teams: names})
}

// TeamNames parses a teams.yaml manifest back to the ordered team names. Used by
// the reconciler (which reads the committed manifest, not the spec).
func TeamNames(manifest []byte) ([]string, error) {
	m, err := unmarshalMap(manifest)
	if err != nil {
		return nil, err
	}
	raw, ok := m["teams"].([]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// defaultTeamQuotaOrder is the RENDER ORDER of the built-in quota entries. It is
// deliberately not alphabetical: it is the order the renderer has always emitted,
// and every instance has that byte sequence committed. Sorting these would show up
// as render drift on every existing instance for no benefit, so the built-ins keep
// their historical order and only ADDITIONAL keys are sorted (see teamQuota).
var defaultTeamQuotaOrder = []string{"services.loadbalancers", "services.nodeports", "pods"}

// defaultTeamQuota is the secure-minimal quota a team gets unless its spec entry
// overrides an entry. Both LoadBalancers and NodePorts are zero: each is a way for
// a team workload to open a public entrypoint of its own, which the landing zone
// wants to be a deliberate, reviewed act rather than a side effect of a Service
// manifest.
var defaultTeamQuota = map[string]string{
	"services.loadbalancers": "0",
	"services.nodeports":     "0",
	"pods":                   "50",
}

// teamQuota merges a team's spec overrides onto the defaults and returns the
// entries in render order: the built-ins first, in defaultTeamQuotaOrder (with any
// overridden VALUE substituted in place), then every additional key sorted. Sorting
// the tail keeps the output deterministic — a Go map range is randomised, and an
// unstable render would fail the committed-manifest drift check on every run.
func teamQuota(overrides map[string]string) [][2]string {
	out := make([][2]string, 0, len(defaultTeamQuota)+len(overrides))
	for _, k := range defaultTeamQuotaOrder {
		v := defaultTeamQuota[k]
		if o, ok := overrides[k]; ok {
			v = o
		}
		out = append(out, [2]string{k, v})
	}
	extra := make([]string, 0, len(overrides))
	for k := range overrides {
		if _, isDefault := defaultTeamQuota[k]; !isDefault {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		out = append(out, [2]string{k, overrides[k]})
	}
	return out
}

// RenderTeamSettings is a team's AplTeamSettingSet — the CR apl-operator
// provisions the namespace + Keycloak group + realm role team-<name> from. Secure
// minimal defaults: no managed monitoring, private-only network policy, a modest
// quota, and self-service scoped to non-destructive actions.
//
// The quota is the one block the spec can tune (spec.teams[].resourceQuota); every
// other field is fixed here and tuned by the platform admin in the console. A team
// with no resourceQuota renders byte-for-byte what it always did.
func RenderTeamSettings(t Team) string {
	var quota strings.Builder
	for _, kv := range teamQuota(t.ResourceQuota) {
		fmt.Fprintf(&quota, "    - name: %s\n      value: %q\n", kv[0], kv[1])
	}
	return fmt.Sprintf(`kind: AplTeamSettingSet
metadata:
  name: %[1]s
  labels:
    apl.io/teamId: %[1]s
spec:
  alerts:
    groupInterval: 5m
    receivers:
      - none
    repeatInterval: 3h
  managedMonitoring:
    alertmanager: false
    grafana: false
  networkPolicy:
    egressPublic: false
    ingressPrivate: true
  resourceQuota:
%[2]s  selfService:
    teamMembers:
      createServices: true
      downloadDockerLogin: true
      downloadKubeconfig: true
      editSecurityPolicies: false
      useCloudShell: true
`, t.Name, quota.String())
}

// RenderTeamApps is a team's AplTeamTool — team-tool resource sizing. Minimal
// requests/limits; the team enables tools + resizes in the console.
func RenderTeamApps(name string) string {
	return fmt.Sprintf(`kind: AplTeamTool
metadata:
  name: %[1]s
  labels:
    apl.io/teamId: %[1]s
spec: {}
`, name)
}
