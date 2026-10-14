package harbor

import (
	"strings"
	"testing"
)

// The flag-set tests that came back with the cobra wiring.

func TestCIHarborProvisionerCmd(t *testing.T) {
	c := HarborProvisionerCmd()
	if c.Use != "harbor-provisioner" {
		t.Errorf("Use = %q", c.Use)
	}
	if !strings.Contains(c.Long, "Kubernetes-auth") {
		t.Error("Long help must describe the k8s-auth OpenBao write path")
	}
}
func TestSeedStandbyHarborRobotsSeedsBoth(t *testing.T) {
	summary := setHarborEnv(t, map[string]string{
		"HARBOR_URL":           "http://env.internal", // http:// stripped by the command
		"EXISTING_ROBOT":       "robot$ci-firewall-controller",
		"EXISTING_SECRET":      "push-secret",
		"EXISTING_PULL_ROBOT":  "robot$pull-platform",
		"EXISTING_PULL_SECRET": "pull-secret",
	})
	bao := withStandbySeams(t)
	c := SeedStandbyHarborRobotsCmd()
	var err error
	out := captureStdout(t, func() { err = c.RunE(c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"secret/harbor/robot username=robot$ci-firewall-controller password=push-secret registry_host=env.internal",
		"secret/harbor/pull-robot username=robot$pull-platform password=pull-secret registry_host=env.internal",
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
