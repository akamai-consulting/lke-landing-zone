package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubKubectl replaces the execOutput seam so a test can drive the kubectl /
// bao orchestration from canned output. Non-kubectl calls fall through to the
// real execOutput (none are expected in these tests).
func stubKubectl(t *testing.T, fn func(args []string) ([]byte, error)) {
	t.Helper()
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })
	execOutput = func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return orig(name, args...)
		}
		return fn(args)
	}
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

// itemsJSON wraps raw item objects in the {"items":[...]} envelope kItems parses.
func itemsJSON(items ...string) []byte {
	return []byte(`{"items":[` + strings.Join(items, ",") + `]}`)
}

func rfc(daysAgo int) string {
	return time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour).Format(time.RFC3339)
}

func setSummary(t *testing.T) {
	t.Helper()
	t.Setenv("REGION", "primary")
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "summary.md"))
}

func TestRunHealthApproleRotation(t *testing.T) {
	t.Run("no success warns but does not fail", func(t *testing.T) {
		setSummary(t)
		stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(), nil })
		captureStdout(t, func() {
			if err := runHealthApproleRotation(100); err != nil {
				t.Errorf("err = %v, want nil (warn-only)", err)
			}
		})
	})

	t.Run("overdue success warns but does not fail", func(t *testing.T) {
		setSummary(t)
		wf := fmt.Sprintf(`{"status":{"phase":"Succeeded","finishedAt":%q}}`, rfc(200))
		stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(wf), nil })
		out := captureStdout(t, func() {
			if err := runHealthApproleRotation(100); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
		if !strings.Contains(out, "200 days ago") {
			t.Errorf("missing age line, got:\n%s", out)
		}
	})

	t.Run("recent success is current", func(t *testing.T) {
		setSummary(t)
		wf := fmt.Sprintf(`{"status":{"phase":"Succeeded","finishedAt":%q}}`, rfc(1))
		stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(wf), nil })
		out := captureStdout(t, func() {
			if err := runHealthApproleRotation(100); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
		if !strings.Contains(out, "is current") {
			t.Errorf("expected current verdict, got:\n%s", out)
		}
	})
}

func TestRunHealthLKEAdminRotation(t *testing.T) {
	t.Run("unreachable cluster skips", func(t *testing.T) {
		setSummary(t)
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "version") {
				return nil, fmt.Errorf("connection refused")
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := runHealthLKEAdminRotation(35, 90); err != nil {
				t.Errorf("err = %v, want nil (skip on unreachable)", err)
			}
		})
	})

	t.Run("past critical SLA fails the job", func(t *testing.T) {
		setSummary(t)
		sec := fmt.Sprintf(`{"metadata":{"name":"lke-admin-token-abc","creationTimestamp":%q}}`, rfc(100))
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "version") {
				return []byte("Client Version: v1.30"), nil
			}
			if argsContain(args, "secrets") {
				return itemsJSON(sec), nil
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := runHealthLKEAdminRotation(35, 90); err == nil {
				t.Error("err = nil, want non-nil past the critical SLA")
			}
		})
	})

	t.Run("fresh token is current", func(t *testing.T) {
		setSummary(t)
		sec := fmt.Sprintf(`{"metadata":{"name":"lke-admin-token-abc","creationTimestamp":%q}}`, rfc(2))
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "version") {
				return []byte("ok"), nil
			}
			if argsContain(args, "secrets") {
				return itemsJSON(sec), nil
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := runHealthLKEAdminRotation(35, 90); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	})
}

func TestRunHealthLokiObjkeyRotation(t *testing.T) {
	t.Run("no token is a non-fatal not-found", func(t *testing.T) {
		setSummary(t)
		t.Setenv("OPENBAO_ROOT_TOKEN", "")
		captureStdout(t, func() {
			if err := runHealthLokiObjkeyRotation(105, 120); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	})

	t.Run("past critical SLA fails the job", func(t *testing.T) {
		setSummary(t)
		t.Setenv("OPENBAO_ROOT_TOKEN", "roottok")
		meta := fmt.Sprintf(`{"data":{"updated_time":%q}}`, rfc(130))
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "metadata") {
				return []byte(meta), nil
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := runHealthLokiObjkeyRotation(105, 120); err == nil {
				t.Error("err = nil, want non-nil past the critical SLA")
			}
		})
	})

	t.Run("fresh key is current", func(t *testing.T) {
		setSummary(t)
		t.Setenv("OPENBAO_ROOT_TOKEN", "roottok")
		meta := fmt.Sprintf(`{"data":{"updated_time":%q}}`, rfc(3))
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "metadata") {
				return []byte(meta), nil
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := runHealthLokiObjkeyRotation(105, 120); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	})
}
