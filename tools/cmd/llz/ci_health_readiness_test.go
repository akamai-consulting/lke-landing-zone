package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// stubBaoExec replaces the RESILIENT bao exec seam for one test. runHealthOpenbao
// reads seal state through baoExecFn (not a bare kubectl exec) so that documented
// transient failures — konnectivity "No agent available" and friends — retry
// instead of being reported as a sealed pod.
func stubBaoExec(t *testing.T, fn func(pod string, args []string) (string, error)) {
	t.Helper()
	prev := baoExecFn
	baoExecFn = func(pod, _, _ string, args ...string) (string, string, error) {
		out, err := fn(pod, args)
		if err != nil {
			return "", err.Error(), err
		}
		return out, "", nil
	}
	t.Cleanup(func() { baoExecFn = prev })
}

func TestRunHealthOpenbao(t *testing.T) {
	t.Run("all unsealed and ESO ready", func(t *testing.T) {
		setSummary(t)
		summaryPath := os.Getenv("GITHUB_STEP_SUMMARY")
		unsealed := `{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`
		stubBaoExec(t, func(string, []string) (string, error) { return unsealed, nil })
		stubKubectl(t, func(args []string) ([]byte, error) {
			switch {
			case argsContain(args, "clustersecretstores"): // CSS Ready jsonpath
				return []byte("True"), nil
			default: // externalsecrets list
				return itemsJSON(), nil
			}
		})
		out := captureStdout(t, func() {
			if err := runHealthOpenbao(); err != nil {
				t.Errorf("err = %v, want nil (warn-only)", err)
			}
		})
		if !strings.Contains(out, "All OpenBao pods unsealed") {
			t.Errorf("expected unsealed verdict, got:\n%s", out)
		}
		body, _ := os.ReadFile(summaryPath)
		if !strings.Contains(string(body), "All ExternalSecrets: Ready") {
			t.Errorf("summary missing ESO-ready line:\n%s", body)
		}
	})

	// The case that had no coverage, and the reason this changed: the exec never
	// answers. That is a connectivity failure, NOT evidence about the seal — and
	// reporting it as "SEALED" sent operators to the openbao-unseal-key Secret,
	// the 32-byte static key and Raft storage, all of which are fine.
	t.Run("an unreadable pod is UNKNOWN, not sealed", func(t *testing.T) {
		setSummary(t)
		stubBaoExec(t, func(string, []string) (string, error) {
			return "", errors.New("error dialing backend: No agent available")
		})
		stubKubectl(t, func(args []string) ([]byte, error) {
			if argsContain(args, "clustersecretstores") {
				return []byte("True"), nil
			}
			return itemsJSON(), nil
		})
		captureStdout(t, func() {
			if err := runHealthOpenbao(); err != nil {
				t.Errorf("err = %v, want nil (warn-only)", err)
			}
		})
		b, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
		body := string(b)
		if strings.Contains(body, "Sealed pods detected") {
			t.Errorf("an exec failure must NOT be reported as sealed — that points the operator at the seal key when the tunnel is the problem:\n%s", body)
		}
		if !strings.Contains(body, "unreadable") {
			t.Errorf("summary should say the pods were unreadable and seal state unknown:\n%s", body)
		}
	})

	// A GENUINELY sealed pod — bao status answers, and says sealed. This subtest
	// used to simulate an exec FAILURE and assert it reported as sealed
	// ("exec fails -> fail-safe sealed default"), which encoded the bug: a
	// konnectivity blip was indistinguishable from a seal problem, and sent the
	// operator to the unseal key and Raft storage. The two cases are now separate.
	t.Run("a sealed pod is reported but never fails", func(t *testing.T) {
		setSummary(t)
		stubBaoExec(t, func(string, []string) (string, error) {
			return `{"initialized":true,"sealed":true,"ha_enabled":false}`, nil
		})
		stubKubectl(t, func(args []string) ([]byte, error) {
			switch {
			case argsContain(args, "clustersecretstores"):
				return []byte(""), nil // NotFound
			default:
				es := `{"metadata":{"namespace":"observability","name":"loki"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}`
				return itemsJSON(es), nil
			}
		})
		body := ""
		captureStdout(t, func() {
			if err := runHealthOpenbao(); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
		b, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
		body = string(b)
		if !strings.Contains(body, "Sealed pods detected") {
			t.Errorf("summary missing sealed warning:\n%s", body)
		}
		if !strings.Contains(body, "Unhealthy ExternalSecrets") {
			t.Errorf("summary missing unhealthy ES section:\n%s", body)
		}
	})
}

func TestRunHealthCertManager(t *testing.T) {
	t.Run("all ready", func(t *testing.T) {
		setSummary(t)
		cert := `{"metadata":{"namespace":"observability","name":"otel"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`
		stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(cert), nil })
		out := captureStdout(t, func() {
			if err := runHealthCertManager(); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
		if !strings.Contains(out, "All cert-manager Certificates Ready") {
			t.Errorf("expected all-ready verdict, got:\n%s", out)
		}
	})

	t.Run("a not-ready cert is warned but does not fail", func(t *testing.T) {
		setSummary(t)
		cert := `{"metadata":{"namespace":"observability","name":"otel"},"status":{"conditions":[{"type":"Ready","status":"False","message":"DNS-01 challenge failed"}]}}`
		stubKubectl(t, func([]string) ([]byte, error) { return itemsJSON(cert), nil })
		captureStdout(t, func() {
			if err := runHealthCertManager(); err != nil {
				t.Errorf("err = %v, want nil (warn-only)", err)
			}
		})
		body, _ := os.ReadFile(os.Getenv("GITHUB_STEP_SUMMARY"))
		if !strings.Contains(string(body), "DNS-01 challenge failed") {
			t.Errorf("summary missing not-ready message:\n%s", body)
		}
	})
}
