package onboard

import (
	"net/url"
	"strings"
	"testing"
)

// Tests that followed their subjects here rather than the subjects being
// exported to reach them. Four symbols were briefly exported to satisfy these
// from package main — that is the anti-pattern this campaign distinguishes from
// a genuine seam: Prompt stayed exported because commands.go really calls it;
// these four had no caller outside a test.

func TestClusterFromEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://us-east-1.linodeobjects.com": "us-east-1",
		"http://nl-ams-1.linodeobjects.com":   "nl-ams-1",
		"us-east-1.linodeobjects.com":         "us-east-1",
		"https://example.com":                 "",
		"":                                    "",
	}
	for in, want := range cases {
		if got := clusterFromEndpoint(in); got != want {
			t.Errorf("clusterFromEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdminFlagAndBanner(t *testing.T) {
	if adminFlag(false) != "" {
		t.Error("adminFlag(false) should be empty")
	}
	if adminFlag(true) != " --admin" {
		t.Errorf("adminFlag(true) = %q, want ' --admin'", adminFlag(true))
	}
	if adminBanner(false) != "" {
		t.Error("adminBanner(false) should be empty")
	}
	if !strings.Contains(adminBanner(true), "[ADMIN:") {
		t.Errorf("adminBanner(true) = %q, want it to contain [ADMIN:", adminBanner(true))
	}
}

func TestGhFineGrainedDispatchURL(t *testing.T) {
	u, err := url.Parse(ghFineGrainedDispatchURL("llz-e2e-dispatch", "my-org"))
	if err != nil {
		t.Fatalf("not a valid URL: %v", err)
	}
	if u.Host != "github.com" || u.Path != "/settings/personal-access-tokens/new" {
		t.Errorf("unexpected host/path: %q", u)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"name": "llz-e2e-dispatch", "target_name": "my-org", "expires_in": "90",
		"contents": "write", "actions": "write", "workflows": "write",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	// owner omitted -> no target_name.
	if q2 := mustQuery(t, ghFineGrainedDispatchURL("n", "")); q2.Has("target_name") {
		t.Errorf("empty owner should omit target_name, got %v", q2)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
