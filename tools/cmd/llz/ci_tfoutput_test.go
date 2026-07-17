package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const clusterOutputs = `{
  "cluster_id":   {"value": "12345", "type": "string", "sensitive": false},
  "kubeconfig_raw": {"value": "apiVersion: v1\nkind: Config\n", "type": "string", "sensitive": true},
  "api_endpoints": {"value": ["https://a:6443", "https://b:6443"], "type": ["list","string"], "sensitive": false}
}`

func TestTFOutputValue(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		asJSON  bool
		allow   bool
		want    string
		wantErr bool
	}{
		{name: "string raw", out: "cluster_id", want: "12345"},
		{name: "string forced json", out: "cluster_id", asJSON: true, want: `"12345"`},
		{name: "complex value is json even raw", out: "api_endpoints", want: `["https://a:6443","https://b:6443"]`},
		{name: "multiline string raw", out: "kubeconfig_raw", want: "apiVersion: v1\nkind: Config\n"},
		{name: "missing errors", out: "nope", wantErr: true},
		{name: "missing allowed is empty", out: "nope", allow: true, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := tfOutputValue(clusterOutputs, c.out, c.asJSON, c.allow)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("value = %q, want %q", got, c.want)
			}
		})
	}
}

// The "No outputs found" hardening: a zero-output state (empty or {} stdout)
// must yield a clean absence, never leak warning text into the value.
func TestTFOutputValue_ZeroOutputState(t *testing.T) {
	for _, blob := range []string{"", "  ", "{}"} {
		if _, err := tfOutputValue(blob, "cluster_id", false, false); err == nil {
			t.Errorf("blob %q: want missing-output error", blob)
		}
		got, err := tfOutputValue(blob, "cluster_id", false, true)
		if err != nil || got != "" {
			t.Errorf("blob %q with --allow-missing: got (%q,%v), want empty", blob, got, err)
		}
	}
}

func TestRunCITFOutput_Destinations(t *testing.T) {
	prev := tfOutputRunFn
	tfOutputRunFn = func() (string, error) { return clusterOutputs, nil }
	t.Cleanup(func() { tfOutputRunFn = prev })

	// --out-key → GITHUB_OUTPUT line.
	outPath := filepath.Join(t.TempDir(), "gho")
	t.Setenv("GITHUB_OUTPUT", outPath)
	if err := runCITFOutput("cluster_id", false, false, "id", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outPath); strings.TrimSpace(string(b)) != "id=12345" {
		t.Errorf("GITHUB_OUTPUT = %q, want id=12345", string(b))
	}

	// A multi-line value must refuse --out-key (would corrupt the file).
	if err := runCITFOutput("kubeconfig_raw", false, false, "kc", ""); err == nil {
		t.Error("multi-line value with --out-key should error")
	}

	// --out-file → the raw value verbatim (kubeconfig case).
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := runCITFOutput("kubeconfig_raw", false, false, "", kc); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(kc); string(b) != "apiVersion: v1\nkind: Config\n" {
		t.Errorf("kubeconfig file = %q", string(b))
	}
}
