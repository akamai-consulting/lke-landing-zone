package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

type fakeKubeconfigClient struct {
	clusters   []map[string]any
	kubeconfig string
	getErr     error
}

func (f *fakeKubeconfigClient) ListClusters(context.Context) ([]map[string]any, error) {
	return f.clusters, nil
}
func (f *fakeKubeconfigClient) GetKubeconfig(context.Context, uint64) (string, error) {
	return f.kubeconfig, f.getErr
}

func withFakeKubeconfig(t *testing.T, fake *fakeKubeconfigClient) {
	t.Helper()
	t.Setenv("LINODE_TOKEN", "tok")
	t.Setenv("LINODE_API_TOKEN", "")
	prev := newKubeconfigClient
	newKubeconfigClient = func(string) kubeconfigClient { return fake }
	t.Cleanup(func() { newKubeconfigClient = prev })
}

func TestFetchKubeconfigWritesDecodedFile(t *testing.T) {
	const raw = "apiVersion: v1\nkind: Config\n"
	fake := &fakeKubeconfigClient{kubeconfig: base64.StdEncoding.EncodeToString([]byte(raw))}
	withFakeKubeconfig(t, fake)

	out := filepath.Join(t.TempDir(), "nested", "kubeconfig")
	if err := runCIFetchKubeconfig(fetchKubeconfigOpts{ref: clusterRef{clusterID: "5"}, output: out}); err != nil {
		t.Fatalf("fetch-kubeconfig = %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if string(got) != raw {
		t.Errorf("kubeconfig = %q, want decoded %q", got, raw)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestFetchKubeconfigMissingErrorsWithoutAllow(t *testing.T) {
	fake := &fakeKubeconfigClient{kubeconfig: ""} // not-ready cluster
	withFakeKubeconfig(t, fake)
	out := filepath.Join(t.TempDir(), "kubeconfig")
	if err := runCIFetchKubeconfig(fetchKubeconfigOpts{ref: clusterRef{clusterID: "5"}, output: out}); err == nil {
		t.Error("empty kubeconfig without --allow-missing should error")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("no file should be written when the kubeconfig is missing")
	}
}

func TestFetchKubeconfigAllowMissingSetsOutput(t *testing.T) {
	fake := &fakeKubeconfigClient{kubeconfig: ""}
	withFakeKubeconfig(t, fake)
	ghaOut := filepath.Join(t.TempDir(), "gha_output")
	t.Setenv("GITHUB_OUTPUT", ghaOut)

	out := filepath.Join(t.TempDir(), "kubeconfig")
	if err := runCIFetchKubeconfig(fetchKubeconfigOpts{ref: clusterRef{clusterID: "5"}, output: out, allowMissing: true}); err != nil {
		t.Fatalf("allow-missing fetch = %v", err)
	}
	b, _ := os.ReadFile(ghaOut)
	if string(b) != "available=false\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want available=false", b)
	}
}

func TestFetchKubeconfigRequiresOutput(t *testing.T) {
	withFakeKubeconfig(t, &fakeKubeconfigClient{})
	if err := runCIFetchKubeconfig(fetchKubeconfigOpts{ref: clusterRef{clusterID: "5"}}); err == nil {
		t.Error("missing --output should error")
	}
}
