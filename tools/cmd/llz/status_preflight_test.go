package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// chdirInstanceRoot puts the test in a directory statusPreflight will accept as an
// instance checkout. `llz status` now refuses to run outside one — every
// remediation it prints (.llz/secrets.env, the <env>.tfvars fetch-kubeconfig
// resolves the cluster through) is relative to the instance root — so the
// cluster-access tests below have to stand somewhere real before they can reach
// the behaviour they are about.
func chdirInstanceRoot(t *testing.T) string {
	t.Helper()
	dir := chdirTempDir(t)
	mustWrite(t, filepath.Join(dir, ".copier-answers.yml"), "instance_repo: acme/inst\n")
	return dir
}

func TestStatusPreflightRefusesOutsideAnInstance(t *testing.T) {
	// The wrong-directory case, answered as wrong-directory. It used to fall through
	// to the kubeconfig block, whose first line greps a .llz/secrets.env that is not
	// there — fifteen lines of unfollowable advice for a one-word mistake.
	origLook := execLookPath
	t.Cleanup(func() { execLookPath = origLook })
	execLookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	chdirTempDir(t) // deliberately NOT an instance root

	err := statusPreflight("lab")
	if err == nil {
		t.Fatal("expected a refusal outside an instance checkout")
	}
	if !strings.Contains(err.Error(), "must run from your instance repo root") {
		t.Errorf("error should name the real problem, got: %v", err)
	}
	// The kubeconfig remediation is for someone standing in an instance; printing it
	// here is what made the message unfollowable. (Naming .llz/secrets.env as part of
	// WHY this directory is wrong is fine — it is the copy-paste block that must not
	// appear.)
	for _, unwanted := range []string{"llz ci fetch-kubeconfig", "apiServerAllowCIDRs", "runner-acl open"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("must not print the in-instance remediation (%q) from outside one:\n%v", unwanted, err)
		}
	}
}

func TestStatusPreflightNoContext(t *testing.T) {
	// A laptop that has never fetched a kubeconfig — the state every adopter is in
	// right after the build, since the cluster was created by GitHub Actions.
	origLook, origOut := execLookPath, execOutput
	t.Cleanup(func() { execLookPath, execOutput = origLook, origOut })
	execLookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	execOutput = func(_ string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("error: current-context is not set")
	}
	chdirInstanceRoot(t)

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
	chdirInstanceRoot(t)

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
	chdirInstanceRoot(t)

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

func TestNoClusterAccessOffersTheACLFixThatIsNotAReApply(t *testing.T) {
	// The default quickstart answer for cluster.apiServerAllowCIDRs is EMPTY —
	// correct for github.com-hosted runners, which open their egress IP per job and
	// revoke it on the way out — so the ACL is routinely a correctly-configured one
	// that has simply never contained this laptop. When the only remedy named was
	// "edit the spec and re-apply", the operator paid a full apply to run one
	// kubectl. `runner-acl open` is the same Linode-API write the CI job does.
	err := noClusterAccessErr("lab", "dial tcp: i/o timeout")

	for _, want := range []string{
		"llz ci runner-acl open --region lab", // the cheap fix, with the env filled in
		"no re-apply",                         // why it is the one to reach for first
		"llz env edit lab",                    // the permanent fix is still offered
		"apiServerAllowCIDRs",                 // and what it is they are editing
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("remediation missing %q:\n%s", want, err)
		}
	}
}

func TestNoClusterAccessDoesNotPushTheConfigMapLeaseByDefault(t *testing.T) {
	// --runner-configmap only matters when the cidrFirewall component is enabled,
	// whose controller REPLACES the ACL on each reconcile. It is DefaultDisabled,
	// so presenting the flag as part of the normal fix would have most operators
	// pass a flag that needs cluster access they do not yet have.
	err := noClusterAccessErr("lab", "connection refused")
	if !strings.Contains(err.Error(), "cidrFirewall") {
		t.Errorf("the flag's precondition must be stated, not just the flag:\n%s", err)
	}
	if strings.Contains(err.Error(), "runner-acl open --region lab --runner-configmap") {
		t.Errorf("--runner-configmap must not be in the copy-paste command:\n%s", err)
	}
}
