package clusterspec

import (
	"strings"
	"testing"
)

func TestRenderTeamsManifest_RoundTrip(t *testing.T) {
	y := RenderTeamsManifest([]Team{{Name: "platform"}, {Name: "gsap"}})
	names, err := TeamNames([]byte(y))
	if err != nil {
		t.Fatalf("TeamNames: %v", err)
	}
	if strings.Join(names, ",") != "platform,gsap" {
		t.Errorf("names = %v, want [platform gsap] (order preserved)", names)
	}
	// No teams → an empty, still-parseable manifest.
	if n, err := TeamNames([]byte(RenderTeamsManifest(nil))); err != nil || len(n) != 0 {
		t.Errorf("empty teams → empty names, got %v (err %v)", n, err)
	}
}

func TestTeamNames_InvalidYAML(t *testing.T) {
	if _, err := TeamNames([]byte("\t: not yaml :\n  - [")); err == nil {
		t.Error("TeamNames must error on invalid YAML")
	}
}

func TestRenderTeamSettings(t *testing.T) {
	y := RenderTeamSettings(Team{Name: "platform"})
	if got, _ := digStr(t, y, "kind"); got != "AplTeamSettingSet" {
		t.Errorf("kind = %q, want AplTeamSettingSet", got)
	}
	if got, _ := digStr(t, y, "metadata", "name"); got != "platform" {
		t.Errorf("metadata.name = %q, want platform", got)
	}
	if got, _ := digStr(t, y, "metadata", "labels", "apl.io/teamId"); got != "platform" {
		t.Errorf("apl.io/teamId label = %q, want platform", got)
	}
	// Must be well-formed YAML apl-operator can consume.
	if _, err := unmarshalMap([]byte(y)); err != nil {
		t.Errorf("settings must be valid YAML: %v\n%s", err, y)
	}
}

func TestRenderTeamApps(t *testing.T) {
	y := RenderTeamApps("platform")
	if got, _ := digStr(t, y, "kind"); got != "AplTeamTool" {
		t.Errorf("kind = %q, want AplTeamTool", got)
	}
	if got, _ := digStr(t, y, "metadata", "name"); got != "platform" {
		t.Errorf("metadata.name = %q, want platform", got)
	}
	if _, err := unmarshalMap([]byte(y)); err != nil {
		t.Errorf("apps must be valid YAML: %v", err)
	}
}

// The exact byte sequence RenderTeamSettings emitted before spec.teams[].resourceQuota
// existed. Every instance in the field has this committed, and `llz lint` compares
// committed render output byte-for-byte against a fresh render — so a team that sets
// no quota must still produce EXACTLY this. Pinning the literal (rather than
// re-deriving it from defaultTeamQuota) is the point: it fails if someone reorders or
// reformats the block, which is precisely the change that would spray drift across
// every existing instance.
const teamSettingsNoQuotaOverride = `kind: AplTeamSettingSet
metadata:
  name: platform
  labels:
    apl.io/teamId: platform
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
`

func TestRenderTeamSettings_NoQuotaIsByteIdenticalToPreSpecOutput(t *testing.T) {
	if got := RenderTeamSettings(Team{Name: "platform"}); got != teamSettingsNoQuotaOverride {
		t.Errorf("a team with no resourceQuota must render unchanged.\ngot:\n%s\nwant:\n%s", got, teamSettingsNoQuotaOverride)
	}
	// Explicitly-empty map is the same as unset — a spec that round-trips through
	// YAML can produce either.
	if got := RenderTeamSettings(Team{Name: "platform", ResourceQuota: map[string]string{}}); got != teamSettingsNoQuotaOverride {
		t.Error("an empty resourceQuota map must render as if unset")
	}
}

func TestRenderTeamSettings_QuotaOverrideReplacesInPlace(t *testing.T) {
	y := RenderTeamSettings(Team{Name: "platform", ResourceQuota: map[string]string{"services.loadbalancers": "1"}})
	if !strings.Contains(y, "- name: services.loadbalancers\n      value: \"1\"") {
		t.Errorf("override not applied:\n%s", y)
	}
	if strings.Contains(y, "value: \"0\"\n    - name: services.loadbalancers") || strings.Count(y, "services.loadbalancers") != 1 {
		t.Errorf("override must REPLACE the default, not duplicate it:\n%s", y)
	}
	// Overriding one entry must not disturb the others.
	if !strings.Contains(y, "- name: services.nodeports\n      value: \"0\"") ||
		!strings.Contains(y, "- name: pods\n      value: \"50\"") {
		t.Errorf("untouched defaults must survive:\n%s", y)
	}
	// Order is unchanged: an override substitutes a value, it does not move the key.
	if idx := strings.Index(y, "services.loadbalancers"); idx > strings.Index(y, "services.nodeports") {
		t.Error("an overridden default must keep its original position")
	}
}

func TestRenderTeamSettings_ExtraKeysAppendedDeterministically(t *testing.T) {
	q := map[string]string{"requests.cpu": "4", "count/deployments.apps": "10", "pods": "99"}
	first := RenderTeamSettings(Team{Name: "platform", ResourceQuota: q})
	// Go map iteration is randomised; an unstable render would fail the committed
	// drift check intermittently. Re-render enough times to catch it.
	for i := 0; i < 50; i++ {
		if got := RenderTeamSettings(Team{Name: "platform", ResourceQuota: q}); got != first {
			t.Fatalf("render is not deterministic across map iteration order:\n%s\n---\n%s", first, got)
		}
	}
	// Defaults first (pods overridden in place), then the additions sorted.
	wantTail := `    - name: services.loadbalancers
      value: "0"
    - name: services.nodeports
      value: "0"
    - name: pods
      value: "99"
    - name: count/deployments.apps
      value: "10"
    - name: requests.cpu
      value: "4"
`
	if !strings.Contains(first, wantTail) {
		t.Errorf("quota block wrong:\n%s", first)
	}
	if _, err := unmarshalMap([]byte(first)); err != nil {
		t.Errorf("must stay valid YAML: %v\n%s", err, first)
	}
}

// The spec decodes through sigs.k8s.io/yaml, which maps YAML keys via the STRUCT
// JSON TAGS — so `resourceQuota` only reaches Team.ResourceQuota because the tag
// says so. Swapping in a yaml.v3-style decoder (which lowercases field names
// instead) would silently drop the whole block: the spec would still load, render
// would still succeed, and the quota would just never change. Decode-and-render is
// the only assertion that catches that, so it belongs in a test.
func TestDecodeTeamResourceQuotaReachesRender(t *testing.T) {
	lz, err := Decode([]byte(`apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: t
spec:
  teams:
    - name: platform
      openbaoSubtree: secret/platform
      resourceQuota:
        services.loadbalancers: "1"
        pods: "100"
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(lz.Spec.Teams) != 1 {
		t.Fatalf("teams = %d, want 1", len(lz.Spec.Teams))
	}
	got := lz.Spec.Teams[0].ResourceQuota
	if got["services.loadbalancers"] != "1" || got["pods"] != "100" {
		t.Fatalf("resourceQuota did not decode: %#v", got)
	}
	// …and survives all the way into the rendered CR.
	y := RenderTeamSettings(lz.Spec.Teams[0])
	if !strings.Contains(y, "- name: services.loadbalancers\n      value: \"1\"") ||
		!strings.Contains(y, "- name: pods\n      value: \"100\"") {
		t.Errorf("decoded quota did not reach the rendered CR:\n%s", y)
	}
	if errs := validateTeams(lz.Spec.Teams); len(errs) != 0 {
		t.Errorf("the documented example must validate, got %v", errs)
	}
}
