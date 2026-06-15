package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIsEnterprise(t *testing.T) {
	for _, v := range []string{"v1.31.9+lke7", "v1.33.1+lke2"} {
		if !isEnterprise(v) {
			t.Errorf("isEnterprise(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"v1.33.1", "v1.34.0", ""} {
		if isEnterprise(v) {
			t.Errorf("isEnterprise(%q) = true, want false", v)
		}
	}
}

// fakeLKEAdmin implements lkeAdminAPI, recording delete-kubeconfig calls.
type fakeLKEAdmin struct {
	k8sVersion string
	versionErr error
	deletes    int
}

func (f *fakeLKEAdmin) ClusterK8sVersion(context.Context, uint64) (string, error) {
	return f.k8sVersion, f.versionErr
}
func (f *fakeLKEAdmin) DeleteKubeconfig(context.Context, uint64) error {
	f.deletes++
	return nil
}

func withLKEAdmin(t *testing.T, fake *fakeLKEAdmin) {
	t.Helper()
	t.Setenv("LINODE_TOKEN", "tok")
	t.Setenv("ROTATION_APPLY", "")
	prev := newLKEAdminClient
	newLKEAdminClient = func(string) lkeAdminAPI { return fake }
	t.Cleanup(func() { newLKEAdminClient = prev })
}

func TestLKEAdminRotateGuardrails(t *testing.T) {
	// Missing token / missing cluster id fail before any API call.
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("ROTATION_APPLY", "")
	err := runCredentialsLKEAdminRotate(&rotatorOpts{}, "123")
	if err == nil || !strings.Contains(err.Error(), "Linode PAT is required") {
		t.Errorf("no token: err = %v, want PAT-required", err)
	}
	t.Setenv("LINODE_TOKEN", "tok")
	err = runCredentialsLKEAdminRotate(&rotatorOpts{}, "")
	if err == nil || !strings.Contains(err.Error(), "cluster ID is required") {
		t.Errorf("no cluster: err = %v, want cluster-required", err)
	}

	// Standard LKE (no +lke suffix) is hard-refused — even on a dry-run.
	fake := &fakeLKEAdmin{k8sVersion: "v1.33.1"}
	withLKEAdmin(t, fake)
	err = runCredentialsLKEAdminRotate(&rotatorOpts{apply: true}, "123")
	if err == nil || !strings.Contains(err.Error(), "not LKE-Enterprise") {
		t.Errorf("standard LKE: err = %v, want enterprise refusal", err)
	}
	if fake.deletes != 0 {
		t.Error("standard LKE must never reach delete-kubeconfig")
	}

	// A version lookup error surfaces.
	withLKEAdmin(t, &fakeLKEAdmin{versionErr: errors.New("boom")})
	if err := runCredentialsLKEAdminRotate(&rotatorOpts{}, "123"); err == nil {
		t.Error("version lookup error must surface")
	}
}

func TestLKEAdminRotateDryRunAndApply(t *testing.T) {
	// Dry-run (default): record printed, no API write.
	fake := &fakeLKEAdmin{k8sVersion: "v1.31.9+lke7"}
	withLKEAdmin(t, fake)
	out := captureStdout(t, func() {
		if err := runCredentialsLKEAdminRotate(&rotatorOpts{}, "123"); err != nil {
			t.Errorf("dry-run: %v", err)
		}
	})
	if fake.deletes != 0 {
		t.Error("dry-run must not call delete-kubeconfig")
	}
	if !strings.Contains(out, `"dry_run":true`) || !strings.Contains(out, `"event":"lke-admin-rotation"`) {
		t.Errorf("dry-run record missing fields:\n%s", out)
	}

	// --apply: exactly one delete-kubeconfig.
	fake = &fakeLKEAdmin{k8sVersion: "v1.31.9+lke7"}
	withLKEAdmin(t, fake)
	out = captureStdout(t, func() {
		if err := runCredentialsLKEAdminRotate(&rotatorOpts{apply: true}, "123"); err != nil {
			t.Errorf("apply: %v", err)
		}
	})
	if fake.deletes != 1 {
		t.Errorf("apply: delete-kubeconfig calls = %d, want 1", fake.deletes)
	}
	if !strings.Contains(out, `"dry_run":false`) {
		t.Errorf("apply record missing dry_run=false:\n%s", out)
	}
}
