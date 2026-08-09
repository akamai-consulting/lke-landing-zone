package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The status-command test, returned to main. It travelled inside
// status_preflight_test.go but its subject is cmdStatus in commands.go, which is
// inside the blocked scaffold mass. Filename-as-subject, twelfth occurrence.

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
