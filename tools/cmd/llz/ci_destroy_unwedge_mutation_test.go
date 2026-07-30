package main

import (
	"errors"
	"strings"
	"testing"
)

// A --region resolution that ERRORS is not the "cluster already reaped" skip: the
// Linode API being unreachable must fail loud, not silently report nothing to
// unwedge (which would let a destroy proceed against a live, wedged cluster).
func TestResolveUnwedgeKubeconfigPropagatesResolverErrors(t *testing.T) {
	t.Setenv("KUBECONFIG_B64", "")
	t.Setenv("KUBECONFIG", "")
	prev := unwedgeResolveKubeconfigFn
	unwedgeResolveKubeconfigFn = func(string) (string, bool, error) {
		return "", false, errors.New("linode api: 500 internal server error")
	}
	t.Cleanup(func() { unwedgeResolveKubeconfigFn = prev })

	path, cleanup, skip, err := resolveUnwedgeKubeconfig("primary")
	if err == nil || !strings.Contains(err.Error(), "500 internal server error") {
		t.Errorf("err = %v, want the resolver error propagated", err)
	}
	if skip {
		t.Error("a resolver error must not be reported as nothing-to-unwedge")
	}
	if path != "" || cleanup != nil {
		t.Errorf("failed resolution must yield no kubeconfig: path=%q cleanup=%v", path, cleanup != nil)
	}
}

// ...and the same call succeeds through to a written kubeconfig when the resolver
// hands back real material, so the error branch is not simply always taken.
func TestResolveUnwedgeKubeconfigWritesResolvedMaterial(t *testing.T) {
	t.Setenv("KUBECONFIG_B64", "")
	t.Setenv("KUBECONFIG", "")
	prev := unwedgeResolveKubeconfigFn
	unwedgeResolveKubeconfigFn = func(string) (string, bool, error) {
		// base64 of a minimal kubeconfig (KubeconfigContent decodes it).
		return "YXBpVmVyc2lvbjogdjEKa2luZDogQ29uZmlnCg==", true, nil
	}
	t.Cleanup(func() { unwedgeResolveKubeconfigFn = prev })

	path, cleanup, skip, err := resolveUnwedgeKubeconfig("primary")
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	if err != nil || skip {
		t.Fatalf("resolved material: err=%v skip=%v", err, skip)
	}
	if path == "" {
		t.Error("a resolved kubeconfig must be spilled to a path")
	}
}
