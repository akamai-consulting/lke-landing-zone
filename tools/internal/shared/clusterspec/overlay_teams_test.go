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
	y := RenderTeamSettings("platform")
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
