package clusterspec

import (
	"strings"
	"testing"
)

func TestValuesIdentity_RepoURLDefaultsToInstanceRepo(t *testing.T) {
	lz := &LandingZone{}
	lz.Spec.Instance.Repo = "acme/platform-instance"
	lz.Spec.Environments = map[string]Environment{"e2e": func() Environment {
		var e Environment
		e.Cluster.Bootstrap.Name = "platform-e2e"
		e.Cluster.Bootstrap.DomainSuffix = "e2e.internal"
		// aplValues.repoURL deliberately omitted — the common `llz env add` shape.
		return e
	}()}
	id := lz.ValuesIdentity("e2e")
	// Must resolve to the instance repo (the copier tfvars-example default this
	// replaces); an empty RepoURL would leave a carved Application pointing nowhere.
	if id.RepoURL != "https://github.com/acme/platform-instance.git" {
		t.Errorf("RepoURL = %q, want the instance-repo default", id.RepoURL)
	}
	// An explicit spec value still wins.
	e := lz.Spec.Environments["e2e"]
	e.Cluster.Bootstrap.AplValues.RepoURL = "https://github.com/acme/values.git"
	lz.Spec.Environments["e2e"] = e
	if id := lz.ValuesIdentity("e2e"); id.RepoURL != "https://github.com/acme/values.git" {
		t.Errorf("explicit RepoURL = %q, want the spec value to win", id.RepoURL)
	}
}

// TestValidateTeams pins the spec.teams surface: name/subtree shape rules and the
// platform-owned namespace reservations.
func TestValidateTeams(t *testing.T) {
	cases := []struct {
		name    string
		teams   []Team
		wantErr string // "" = valid
	}{
		{"unset is valid", nil, ""},
		{"one team is valid", []Team{{Name: "gsap", OpenbaoSubtree: "secret/gsap"}}, ""},
		{"two teams are valid", []Team{
			{Name: "gsap", OpenbaoSubtree: "secret/gsap"},
			{Name: "web", OpenbaoSubtree: "secret/web"},
		}, ""},
		{"missing name", []Team{{OpenbaoSubtree: "secret/gsap"}}, "name is required"},
		{"bad name", []Team{{Name: "Gsap Team", OpenbaoSubtree: "secret/gsap"}}, "name"},
		{"reserved name admin", []Team{{Name: "admin", OpenbaoSubtree: "secret/admin"}}, "reserved"},
		{"reserved name platform-admin", []Team{{Name: "platform-admin", OpenbaoSubtree: "secret/pa"}}, "reserved"},
		{"trailing-hyphen name rejected", []Team{{Name: "foo-", OpenbaoSubtree: "secret/foo"}}, "must not end with '-'"},
		{"system namespace linode rejected", []Team{{Name: "x", OpenbaoSubtree: "secret/linode"}}, "platform-owned"},
		{"system namespace nested harbor rejected", []Team{{Name: "y", OpenbaoSubtree: "secret/harbor/robots"}}, "platform-owned"},
		{"non-system namespace ok", []Team{{Name: "zapp", OpenbaoSubtree: "secret/zapp"}}, ""},
		{"nested path ok", []Team{{Name: "zapp", OpenbaoSubtree: "secret/zapp/build"}}, ""},
		{"double slash rejected", []Team{{Name: "xteam", OpenbaoSubtree: "secret//linode"}}, "lowercase kebab"},
		{"dotdot traversal rejected", []Team{{Name: "xteam", OpenbaoSubtree: "secret/../linode"}}, "lowercase kebab"},
		{"trailing space rejected", []Team{{Name: "xteam", OpenbaoSubtree: "secret/linodex "}}, "lowercase kebab"},
		{"uppercase rejected", []Team{{Name: "xteam", OpenbaoSubtree: "secret/Zapp"}}, "lowercase kebab"},
		{"linode-lookalike allowed", []Team{{Name: "xteam", OpenbaoSubtree: "secret/linodex"}}, ""},
		{"missing subtree", []Team{{Name: "gsap"}}, "openbaoSubtree is required"},
		{"subtree not under secret/", []Team{{Name: "gsap", OpenbaoSubtree: "kv/gsap"}}, "must begin with 'secret/'"},
		{"glob subtree rejected", []Team{{Name: "gsap", OpenbaoSubtree: "secret/gsap/*"}}, "not a glob"},
		{"trailing-slash subtree rejected", []Team{{Name: "gsap", OpenbaoSubtree: "secret/gsap/"}}, "must not end with '/'"},
		{"bare secret/ mount rejected", []Team{{Name: "gsap", OpenbaoSubtree: "secret/"}}, "must not end with '/'"},
		{"duplicate name", []Team{
			{Name: "gsap", OpenbaoSubtree: "secret/gsap"},
			{Name: "gsap", OpenbaoSubtree: "secret/other"},
		}, "duplicate name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateTeams(tc.teams)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), tc.wantErr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
}

func TestTeamAplRole(t *testing.T) {
	if r := (Team{Name: "gsap"}).AplRole(); r != "team-gsap" {
		t.Errorf("AplRole() = %q, want team-gsap (the apl-core realm role/group)", r)
	}
}

func TestValidateAlerting(t *testing.T) {
	cases := []struct {
		name    string
		a       Alerting
		wantErr string // "" = valid
	}{
		{"unset is valid", Alerting{}, ""},
		{"none is valid", Alerting{Receivers: []string{"none"}}, ""},
		{"slack with channels is valid", Alerting{
			Receivers: []string{"slack"},
			Slack:     AlertingSlack{Channel: "a", ChannelCrit: "b"},
		}, ""},
		{"msteams rejected", Alerting{Receivers: []string{"msteams"}}, "not supported"},
		{"none plus slack rejected", Alerting{Receivers: []string{"none", "slack"}}, "cannot be combined"},
		{"channels without slack receiver rejected", Alerting{
			Slack: AlertingSlack{Channel: "a"},
		}, "does not include slack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateAlerting(tc.a)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), tc.wantErr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
}
