package main

import (
	"os"
	"strings"
	"testing"
)

func TestRunHealthOpenbao(t *testing.T) {
	t.Run("all unsealed and ESO ready", func(t *testing.T) {
		setSummary(t)
		summaryPath := os.Getenv("GITHUB_STEP_SUMMARY")
		unsealed := `{"initialized":true,"sealed":false,"is_self":true,"ha_enabled":true}`
		stubKubectl(t, func(args []string) ([]byte, error) {
			switch {
			case argsContain(args, "status"): // bao status exec
				return []byte(unsealed), nil
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

	t.Run("a sealed pod is reported but never fails", func(t *testing.T) {
		setSummary(t)
		stubKubectl(t, func(args []string) ([]byte, error) {
			switch {
			case argsContain(args, "status"):
				return nil, os.ErrNotExist // exec fails -> fail-safe sealed default
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
