package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestHarborProvisionerNamesTheUnusableHarborHostItIgnores(t *testing.T) {
	for _, tc := range []struct{ host, wantNote string }{
		{"harbor.", `HARBOR_HOST is "harbor." — not a usable registry host`},
		{"", ""},
	} {
		t.Run("host="+tc.host, func(t *testing.T) {
			srv, _ := harborStub(t, http.StatusCreated, []int{http.StatusCreated, http.StatusCreated})
			setProvisionerEnv(t, "adminpass", &fakeBaoStore{})
			t.Setenv("HARBOR_API_URL", srv.URL)
			t.Setenv("HARBOR_HOST", tc.host)

			out := captureStdout(t, func() {
				if err := runCIHarborProvisioner(); err != nil {
					t.Fatal(err)
				}
			})
			if tc.wantNote != "" && !strings.Contains(out, tc.wantNote) {
				t.Errorf("stdout must name the ignored value %q:\n%s", tc.host, out)
			}
			if tc.wantNote == "" && strings.Contains(out, "not a usable registry host") {
				t.Errorf("an absent HARBOR_HOST has no value to complain about:\n%s", out)
			}
			if !strings.Contains(out, "discovered registry host") {
				t.Errorf("stdout missing the discovery note:\n%s", out)
			}
		})
	}
}

// The re-publication loop is the recovery path for a publish that failed after
// the OpenBao seed. It must walk EVERY missing secret — stopping after the
// first success strands the rest of the standby channel.
func TestRepublishMissingRepoSecretsPublishesAllFourAndReportsConverged(t *testing.T) {
	store := &fakeBaoStore{data: map[string]map[string]string{
		"secret/harbor/robot":      {"username": "robot$ci", "password": "p1"},
		"secret/harbor/pull-robot": {"username": "robot$pull", "password": "p2"},
	}}
	t.Setenv("GH_TOKEN", "ghp_test")
	t.Setenv("GH_REPO", "acme/platform")

	origPub, origExists := ghPublishRepoSecret, ghRepoSecretExists
	var published []string
	ghPublishRepoSecret = func(name, value string) error {
		published = append(published, name+"="+value)
		return nil
	}
	ghRepoSecretExists = func(string) (bool, error) { return false, nil } // all four absent
	t.Cleanup(func() { ghPublishRepoSecret, ghRepoSecretExists = origPub, origExists })

	out := captureStdout(t, func() {
		if err := republishMissingRepoSecrets(context.Background(), store); err != nil {
			t.Fatalf("re-publication must succeed: %v", err)
		}
	})
	want := []string{
		"HARBOR_ROBOT_NAME=robot$ci",
		"HARBOR_PASSWORD=p1",
		"HARBOR_PULL_ROBOT_NAME=robot$pull",
		"HARBOR_PULL_PASSWORD=p2",
	}
	if strings.Join(published, " | ") != strings.Join(want, " | ") {
		t.Errorf("published %v,\nwant %v", published, want)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("the loop must run to completion and report converged:\n%s", out)
	}
}
