package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `key = ""` is the degenerate quoted value: len(val) is exactly 2 and the closing
// quote sits at offset 0 of val[1:]. Both boundaries are load-bearing — either
// slip returns the raw two-character `""` instead of the empty string, and a
// malformed-but-non-empty value defeats every `== ""` guard downstream.
func TestTfvarsValueEmptyQuotedValue(t *testing.T) {
	if got := tfvarsValue(`obj_cluster = ""`, "obj_cluster"); got != "" {
		t.Errorf(`tfvarsValue(key = "") = %q, want "" (both quotes stripped)`, got)
	}
	// One more character in and the same two boundaries must still strip.
	if got := tfvarsValue(`obj_cluster = "x"`, "obj_cluster"); got != "x" {
		t.Errorf(`tfvarsValue(key = "x") = %q, want x`, got)
	}
	// An unterminated quote falls through to the raw value (no panic, no strip).
	if got := tfvarsValue(`obj_cluster = "oops`, "obj_cluster"); got != `"oops` {
		t.Errorf("unterminated quote = %q, want the raw value", got)
	}
}

// A spec that carries BOTH a domainSuffix and managedAppPlatform must use the
// spec's domainSuffix; the in-cluster discovery is the no-domainSuffix fallback
// only. It is not a harmless extra read either — the discovery ASSIGNS domain, so
// reaching it on a managed cluster with a pinned suffix (or off-cluster, where it
// returns "") blanks the suffix and fails the preflight outright.
func TestResolveHarborURLSpecDomainWinsOverManagedDiscovery(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("landingzone.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`)
	write("environments/e2e.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: e2e }
spec:
  cluster:
    clusterLabel: c-e2e
    region: us-sea
    bootstrap:
      name: b-e2e
      domainSuffix: pinned.example.com
      managedAppPlatform: true
    objectStorage: { cluster: us-sea-1 }
`)
	t.Chdir(dir)
	t.Setenv("HARBOR_URL", "")
	envFile := withGHAEnvFile(t)
	// Any cluster read here is a bug; fail loudly rather than shelling out.
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		t.Errorf("must not read the cluster when the spec pins a domainSuffix: %s %v", name, args)
		return nil, errors.New("no cluster")
	})

	var err error
	out := captureStdout(t, func() { err = runCIResolveHarborURL("e2e") })
	if err != nil {
		t.Fatalf("a pinned domainSuffix must resolve offline: %v", err)
	}
	if strings.Contains(out, "discovered domain") {
		t.Errorf("the managed discovery branch must not be entered:\n%s", out)
	}
	if !ghaEnvContains(t, envFile, "HARBOR_URL=harbor.pinned.example.com") {
		t.Error("HARBOR_URL must be derived from the spec's domainSuffix")
	}
}
