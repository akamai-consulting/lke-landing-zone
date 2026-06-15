package main

import (
	"strings"
	"testing"
)

func TestPreCommitShimContainsAbsPath(t *testing.T) {
	const llz = "/Users/op/go/bin/llz"
	shim := preCommitShim(llz)

	if !strings.HasPrefix(shim, "#!/usr/bin/env bash") {
		t.Error("shim missing shebang")
	}
	// The absolute path must drive both the primary exec and the -x guard.
	if c := strings.Count(shim, llz); c < 2 {
		t.Errorf("shim references %q %d times, want >=2", llz, c)
	}
	if !strings.Contains(shim, "precommit") {
		t.Error("shim must invoke `llz precommit`")
	}
	// PATH fallback so the hook survives the binary moving.
	if !strings.Contains(shim, "command -v llz") {
		t.Error("shim missing PATH fallback")
	}
}

func TestIsSecretPath(t *testing.T) {
	blocked := []string{
		"certs/server.pem", "ca.der", "tls.key", "store.p12", "store.pfx",
		"terraform.tfstate", "terraform.tfstate.backup",
		"kubeconfig", "clusters/kubeconfig.yaml",
		"terraform/cluster/.terraform/foo",
	}
	for _, p := range blocked {
		if !isSecretPath(p) {
			t.Errorf("expected %q to be blocked", p)
		}
	}
	allowed := []string{
		"terraform/cluster/main.tf", "docs/extending-llz.md",
		"README.md", "my-keychain.go", // "key" only in the middle — not a .key file
	}
	for _, p := range allowed {
		if isSecretPath(p) {
			t.Errorf("expected %q to be allowed", p)
		}
	}
}
