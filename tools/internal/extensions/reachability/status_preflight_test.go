package reachability

import (
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func TestStatusPreflightNoContext(t *testing.T) {
	// A laptop that has never fetched a kubeconfig — the state every adopter is in
	// right after the build, since the cluster was created by GitHub Actions.
	origLook, origOut := kubectlprobe.LookPathFn, kubectlprobe.Exec
	t.Cleanup(func() { kubectlprobe.LookPathFn, kubectlprobe.Exec = origLook, origOut })
	kubectlprobe.LookPathFn = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	kubectlprobe.Exec = func(_ string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("error: current-context is not set")
	}

	err := StatusPreflight("lab")
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
	origLook, origOut, origReach := kubectlprobe.LookPathFn, kubectlprobe.Exec, clusterReachable
	t.Cleanup(func() { kubectlprobe.LookPathFn, kubectlprobe.Exec, clusterReachable = origLook, origOut, origReach })
	kubectlprobe.LookPathFn = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	kubectlprobe.Exec = func(_ string, _ ...string) ([]byte, error) { return []byte("lke123-ctx"), nil }
	clusterReachable = func() (string, bool) { return "dial tcp: i/o timeout", false }

	err := StatusPreflight("lab")
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
	origLook, origOut, origReach := kubectlprobe.LookPathFn, kubectlprobe.Exec, clusterReachable
	t.Cleanup(func() { kubectlprobe.LookPathFn, kubectlprobe.Exec, clusterReachable = origLook, origOut, origReach })
	kubectlprobe.LookPathFn = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	kubectlprobe.Exec = func(_ string, _ ...string) ([]byte, error) { return []byte("lke123-ctx"), nil }
	clusterReachable = func() (string, bool) { return "", true }

	if err := StatusPreflight("lab"); err != nil {
		t.Fatalf("a reachable cluster must pass: %v", err)
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
