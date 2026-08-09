package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The flag-set tests that came back with the cobra wiring.

func TestCIHarborProvisionerCmd(t *testing.T) {
	c := ciHarborProvisionerCmd()
	if c.Use != "harbor-provisioner" {
		t.Errorf("Use = %q", c.Use)
	}
	if !strings.Contains(c.Long, "Kubernetes-auth") {
		t.Error("Long help must describe the k8s-auth OpenBao write path")
	}
}

// httptestNewSmoke401 serves 401 to the smoke's project list.
func httptestNewSmoke401(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/v2.0/projects") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSeedStandbyHarborRobotsSeedsBoth(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"HARBOR_URL":           "http://harbor.env.internal", // http:// stripped by the command
		"EXISTING_ROBOT":       "robot$ci-firewall-controller",
		"EXISTING_SECRET":      "push-secret",
		"EXISTING_PULL_ROBOT":  "robot$pull-platform",
		"EXISTING_PULL_SECRET": "pull-secret",
	})
	bao := withStandbySeams(t)
	c := ciSeedStandbyHarborRobotsCmd()
	var err error
	out := captureStdout(t, func() { err = c.RunE(c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"secret/harbor/robot username=robot$ci-firewall-controller password=push-secret registry_host=harbor.env.internal",
		"secret/harbor/pull-robot username=robot$pull-platform password=pull-secret registry_host=harbor.env.internal",
	}
	if strings.Join(*bao, " | ") != strings.Join(want, " | ") {
		t.Errorf("bao calls = %v, want %v", *bao, want)
	}
	if !strings.Contains(out, "secret/harbor/robot and secret/harbor/pull-robot seeded on the standby peer.") {
		t.Errorf("stdout %q missing the seeded confirmation", out)
	}
	if got := readSummary(t, summary); got != "" {
		t.Errorf("full seed must not write summary notes, got %q", got)
	}
}
