package healthsla

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// readyItem is one Ready:True resource, for fixtures that need the list to be
// NON-EMPTY. An all-clear over zero items is not an all-clear, so any test
// asserting "all Ready" has to supply something for the check to have read.
func readyItem(ns, name string) string {
	return `{"metadata":{"namespace":"` + ns + `","name":"` + name + `"},` +
		`"status":{"conditions":[{"type":"Ready","status":"True"}]}}`
}

func rfc(daysAgo int) string {
	return time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour).Format(time.RFC3339)
}

func setSummary(t *testing.T) {
	t.Helper()
	t.Setenv("REGION", "primary")
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "summary.md"))
}

func TestRunHealthLKEAdminRotation(t *testing.T) {
	t.Run("unreachable cluster skips", func(t *testing.T) {
		setSummary(t)
		ensureDeps(t)
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "version") {
				return nil, fmt.Errorf("connection refused")
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := RunLKEAdminRotation(td, 35, 90); err != nil {
				t.Errorf("err = %v, want nil (skip on unreachable)", err)
			}
		})
	})

	t.Run("past critical SLA fails the job", func(t *testing.T) {
		setSummary(t)
		ensureDeps(t)
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
			if err := RunLKEAdminRotation(td, 35, 90); err == nil {
				t.Error("err = nil, want non-nil past the critical SLA")
			}
		})
	})

	t.Run("fresh token is current", func(t *testing.T) {
		setSummary(t)
		ensureDeps(t)
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
			if err := RunLKEAdminRotation(td, 35, 90); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	})
}
