package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearPATEnv blanks every input the gh-pat-expiry command reads so a test
// starts from a known (all-unset) state regardless of the runner's environment.
func clearPATEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"OPENBAO_SECRETS_WRITE_TOKEN", "APL_VALUES_REPO_TOKEN", "GITHUB_API"} {
		t.Setenv(v, "")
	}
}

func TestRunCIGHPATExpiry(t *testing.T) {
	orig := ghPATProbe
	t.Cleanup(func() { ghPATProbe = orig })

	futureHdr := time.Now().Add(60*24*time.Hour + 12*time.Hour).Format("2006-01-02 15:04:05 -0700")

	t.Run("healthy token passes, summary written", func(t *testing.T) {
		clearPATEnv(t)
		t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "secret-token")
		summary := filepath.Join(t.TempDir(), "summary.md")
		t.Setenv("GITHUB_STEP_SUMMARY", summary)
		ghPATProbe = func(_, _ string) (int, string, error) { return 200, futureHdr, nil }

		captureStdout(t, func() {
			if err := runCIGHPATExpiry(90, 14); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
		body, _ := os.ReadFile(summary)
		if !strings.Contains(string(body), "✅ 60d left") {
			t.Errorf("summary missing healthy row, got:\n%s", body)
		}
	})

	t.Run("never-expiring token fails the gate", func(t *testing.T) {
		clearPATEnv(t)
		t.Setenv("APL_VALUES_REPO_TOKEN", "secret-token")
		t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "summary.md"))
		ghPATProbe = func(_, _ string) (int, string, error) { return 200, "", nil } // no expiry header

		captureStdout(t, func() {
			if err := runCIGHPATExpiry(90, 14); err == nil {
				t.Error("err = nil, want non-nil for a never-expiring PAT")
			}
		})
	})

	t.Run("unreachable probe is a warning, not a failure", func(t *testing.T) {
		clearPATEnv(t)
		t.Setenv("OPENBAO_SECRETS_WRITE_TOKEN", "secret-token")
		t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "summary.md"))
		ghPATProbe = func(_, _ string) (int, string, error) { return 0, "", os.ErrDeadlineExceeded }

		captureStdout(t, func() {
			if err := runCIGHPATExpiry(90, 14); err != nil {
				t.Errorf("err = %v, want nil (unreachable is non-failing)", err)
			}
		})
	})

	t.Run("all tokens unset passes", func(t *testing.T) {
		clearPATEnv(t)
		t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "summary.md"))
		ghPATProbe = func(_, _ string) (int, string, error) {
			t.Error("probe should not be called when no token is set")
			return 0, "", nil
		}
		captureStdout(t, func() {
			if err := runCIGHPATExpiry(90, 14); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	})
}
