package main

import "testing"

func TestRegionFromCluster(t *testing.T) {
	cases := map[string]string{
		"us-ord-1":     "us-ord",
		"us-ord-10":    "us-ord",
		"us-iad-18":    "us-iad",
		"eu-central-1": "eu-central",
		"single":       "single", // no hyphen → returned unchanged
	}
	for in, want := range cases {
		if got := regionFromCluster(in); got != want {
			t.Errorf("regionFromCluster(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"akamai-consulting/lke-landing-zone-example": "lke-landing-zone-example",
		"Org/My-Repo": "my-repo",
		"bare":        "bare",
	}
	for in, want := range cases {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveInstanceRepo(t *testing.T) {
	// Explicit flag always wins.
	if r, err := resolveInstanceRepo("owner/explicit", false); err != nil || r != "owner/explicit" {
		t.Fatalf("flag: got (%q,%v), want owner/explicit", r, err)
	}
	// Admin with no flag and no answers file falls back to the example repo.
	if r, err := resolveInstanceRepo("", true); err != nil || r != defaultTemplateOrg+"/"+templateName+"-example" {
		t.Fatalf("admin default: got (%q,%v)", r, err)
	}
	// Non-admin with no flag and (presumably) no .copier-answers.yml here errors.
	if _, err := resolveInstanceRepo("", false); err == nil {
		t.Errorf("expected error when no repo can be determined")
	}
}
