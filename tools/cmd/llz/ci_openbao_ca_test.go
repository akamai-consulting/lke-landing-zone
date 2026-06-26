package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCIExtractOpenbaoCAPresent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output")
	t.Setenv("GITHUB_OUTPUT", out)
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "secret openbao-tls") {
			t.Errorf("unexpected exec: %s %v", name, args)
		}
		return []byte("Y2VydA==\n"), nil // base64 of the public ca.crt
	})
	if err := runCIExtractOpenbaoCA(false); err != nil {
		t.Fatalf("extract (present): %v", err)
	}
	b, _ := os.ReadFile(out)
	got := string(b)
	if !strings.Contains(got, "ca_b64=Y2VydA==") || !strings.Contains(got, "ca_available=true") {
		t.Errorf("GITHUB_OUTPUT = %q, want ca_b64 + ca_available=true", got)
	}
}

func TestRunCIExtractOpenbaoCAAbsent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required bool
		wantErr  bool
	}{
		{"non-fatal warns + exit 0", false, false},
		{"required errors", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "output")
			t.Setenv("GITHUB_OUTPUT", out)
			withExecOutput(t, func(string, ...string) ([]byte, error) {
				return nil, errors.New("NotFound") // openbao-tls absent
			})
			err := runCIExtractOpenbaoCA(tc.required)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			b, _ := os.ReadFile(out)
			if got := string(b); !strings.Contains(got, "ca_available=false") || strings.Contains(got, "ca_b64=") {
				t.Errorf("GITHUB_OUTPUT = %q, want ca_available=false and no ca_b64", got)
			}
		})
	}
}

func TestExtractOpenbaoCAWiring(t *testing.T) {
	c := ciExtractOpenbaoCACmd()
	if c.Use != "extract-openbao-ca" {
		t.Errorf("Use = %q, want extract-openbao-ca", c.Use)
	}
	if c.Flags().Lookup("required") == nil {
		t.Error("missing --required flag")
	}
}

// withKubectlApply swaps the kubectlApplyFn seam, capturing the applied manifest.
func withKubectlApply(t *testing.T) *string {
	t.Helper()
	var applied string
	prev := kubectlApplyFn
	kubectlApplyFn = func(manifest string) error { applied = manifest; return nil }
	t.Cleanup(func() { kubectlApplyFn = prev })
	return &applied
}

func TestRunCIProvisionPeerCA(t *testing.T) {
	// "Y2VydA==" is base64 of "cert"; the applied Secret must carry that same
	// base64 under data."ca.crt" (genericSecretManifest re-encodes the decoded PEM).
	t.Setenv("CA_B64", "Y2VydA==")
	applied := withKubectlApply(t)
	if err := runCIProvisionPeerCA(globalOpts{}); err != nil {
		t.Fatalf("provision-peer-ca: %v", err)
	}
	if !strings.Contains(*applied, "name: openbao-peer-tls") ||
		!strings.Contains(*applied, "namespace: llz-openbao") ||
		!strings.Contains(*applied, "ca.crt: Y2VydA==") {
		t.Errorf("applied manifest missing expected fields:\n%s", *applied)
	}
}

func TestRunCIProvisionPeerCAGuards(t *testing.T) {
	t.Run("empty CA_B64 refuses", func(t *testing.T) {
		t.Setenv("CA_B64", "")
		applied := withKubectlApply(t)
		if err := runCIProvisionPeerCA(globalOpts{}); err == nil {
			t.Error("empty CA_B64 must error (refuse to provision an empty ca.crt)")
		}
		if *applied != "" {
			t.Error("must not apply anything on empty CA_B64")
		}
	})
	t.Run("invalid base64 errors", func(t *testing.T) {
		t.Setenv("CA_B64", "!!not base64!!")
		applied := withKubectlApply(t)
		if err := runCIProvisionPeerCA(globalOpts{}); err == nil {
			t.Error("invalid base64 must error")
		}
		if *applied != "" {
			t.Error("must not apply anything on invalid base64")
		}
	})
	t.Run("dry-run applies nothing", func(t *testing.T) {
		t.Setenv("CA_B64", "Y2VydA==")
		applied := withKubectlApply(t)
		if err := runCIProvisionPeerCA(globalOpts{dryRun: true}); err != nil {
			t.Fatal(err)
		}
		if *applied != "" {
			t.Error("dry-run must not apply")
		}
	})
}
