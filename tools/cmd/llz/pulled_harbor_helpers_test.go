package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

// Helpers the moved tests use, copied across the new package boundary.
// Copied, not shared: each takes a *testing.T.

// withGHAEnvFile captures $GITHUB_ENV writes; returns the path.
func withGHAEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", p)
	return p
}

func ghaEnvContains(t *testing.T, path, want string) bool {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Contains(string(b), want)
}

func setHarborEnv(t *testing.T, vars map[string]string) (summaryPath string) {
	t.Helper()
	summaryPath = filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)
	t.Setenv("GITHUB_ACTIONS", "1") // ghsecret.Mask emits ::add-mask:: like the script's CI runs
	for _, v := range harborEnvVars {
		t.Setenv(v, vars[v])
	}
	return summaryPath
}

// withStandbySeams swaps the OpenBao root-token seed seam, recording calls.
func withStandbySeams(t *testing.T) (bao *[]string) {
	t.Helper()
	origBao := baoread.KVPut
	bao = new([]string)
	baoread.KVPut = func(path string, fields map[string]string) error {
		*bao = append(*bao, fmt.Sprintf("%s username=%s password=%s registry_host=%s",
			path, fields["username"], fields["password"], fields["registry_host"]))
		return nil
	}
	t.Cleanup(func() { baoread.KVPut = origBao })
	return bao
}

func readSummary(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// harborEnvVars is the standby command's full env contract; setHarborEnv pins
// every one (empty unless the test overrides) so ambient CI values can't leak in.
var harborEnvVars = []string{
	"REGION", "HA_ROLE", "HARBOR_URL", "HARBOR_API_URL",
	"EXISTING_ROBOT", "EXISTING_SECRET", "EXISTING_PULL_ROBOT", "EXISTING_PULL_SECRET",
	"OPENBAO_ROOT_TOKEN",
}
