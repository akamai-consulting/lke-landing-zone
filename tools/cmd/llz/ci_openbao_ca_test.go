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
