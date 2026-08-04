package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusPreflightNoContext(t *testing.T) {
	// A laptop that has never fetched a kubeconfig — the state every adopter is in
	// right after the build, since the cluster was created by GitHub Actions.
	origLook, origOut := execLookPath, execOutput
	t.Cleanup(func() { execLookPath, execOutput = origLook, origOut })
	execLookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	execOutput = func(_ string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("error: current-context is not set")
	}

	err := statusPreflight("lab")
	if err == nil {
		t.Fatal("expected a refusal with no kubectl context")
	}
	for _, want := range []string{"fetch-kubeconfig --region lab", "KUBECONFIG", "apiServerAllowCIDRs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestStatusPreflightUnreachableClusterKeepsKubectlsDiagnosis(t *testing.T) {
	origLook, origOut, origReach := execLookPath, execOutput, clusterReachable
	t.Cleanup(func() { execLookPath, execOutput, clusterReachable = origLook, origOut, origReach })
	execLookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	execOutput = func(_ string, _ ...string) ([]byte, error) { return []byte("lke123-ctx"), nil }
	clusterReachable = func() (string, bool) { return "dial tcp: i/o timeout", false }

	err := statusPreflight("lab")
	if err == nil {
		t.Fatal("expected a refusal when the cluster is unreachable")
	}
	// kubectl's own words survive: "i/o timeout" vs "connection refused" is what
	// tells an ACL problem apart from a wrong context.
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error %q dropped kubectl's diagnosis", err)
	}
}

func TestStatusPreflightPasses(t *testing.T) {
	origLook, origOut, origReach := execLookPath, execOutput, clusterReachable
	t.Cleanup(func() { execLookPath, execOutput, clusterReachable = origLook, origOut, origReach })
	execLookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	execOutput = func(_ string, _ ...string) ([]byte, error) { return []byte("lke123-ctx"), nil }
	clusterReachable = func() (string, bool) { return "", true }

	if err := statusPreflight("lab"); err != nil {
		t.Fatalf("a reachable cluster must pass: %v", err)
	}
}

func TestCmdStatusKeepsTheRootTokenNagWithoutClusterAccess(t *testing.T) {
	// `llz status` promises to flag a lingering OPENBAO_ROOT_TOKEN on EVERY run.
	// That check reads GitHub, not the cluster — so gating it behind cluster
	// reachability would silence it for exactly the operator who has no
	// kubeconfig, which is most of them right after the build that printed it.
	withLookPath(t, func(f string) (string, error) { return "/usr/bin/" + f, nil })
	dir := chdirTempDir(t)
	mustWrite(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: acme/inst\n")
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name == "gh" {
			return []byte(`{"secrets":[{"name":"OPENBAO_ROOT_TOKEN"}]}`), nil
		}
		return nil, errors.New("The connection to the server was refused")
	})

	var err error
	out := captureStdout(t, func() { err = cmdStatus([]string{"lab"}, globalOpts{}, false, 0) })
	if err == nil {
		t.Fatal("unreachable cluster must still fail the command")
	}
	if !strings.Contains(out, "OPENBAO_ROOT_TOKEN is still set in infra-lab") {
		t.Errorf("the standing root-token nag was lost:\n%s", out)
	}
}
