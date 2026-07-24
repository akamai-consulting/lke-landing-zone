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

import "fmt"

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

// RenderTeamSettings is a team's AplTeamSettingSet — the CR apl-operator
// provisions the namespace + Keycloak group + realm role team-<name> from. Secure
// minimal defaults: no managed monitoring, private-only network policy, a modest
// quota, and self-service scoped to non-destructive actions.
func RenderTeamSettings(name string) string {
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
    - name: services.loadbalancers
      value: "0"
    - name: services.nodeports
      value: "0"
    - name: pods
      value: "50"
  selfService:
    teamMembers:
      createServices: true
      downloadDockerLogin: true
      downloadKubeconfig: true
      editSecurityPolicies: false
      useCloudShell: true
`, name)
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
