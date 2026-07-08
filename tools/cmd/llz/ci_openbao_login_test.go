package main

import "testing"

func TestOpenBaoLoginRequiresRepo(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "")
	if err := runOpenBaoLogin(globalOpts{}, "platform-ci", "", "OPENBAO_TOKEN"); err == nil {
		t.Fatal("expected an error when GITHUB_REPOSITORY is unset")
	}
}

func TestOpenBaoLoginDryRun(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "akamai/gsap-apl")
	// dry-run must not attempt to mint an OIDC token or reach OpenBao.
	if err := runOpenBaoLogin(globalOpts{dryRun: true}, "platform-ci", "", "OPENBAO_TOKEN"); err != nil {
		t.Fatalf("dry-run should be a no-op success: %v", err)
	}
}
