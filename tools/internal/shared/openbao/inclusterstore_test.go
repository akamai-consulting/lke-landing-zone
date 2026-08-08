package openbao

// inclusterstore_test.go — OpenInClusterStore's refusals.
//
// It moved here from cmd/llz/openbao_k8s_login.go with no tests: package main's
// only coverage of it was a helper comment noting that something delegated to it.
// The happy path needs a live OpenBao and a mounted ServiceAccount token, so what
// is testable is every way it declines to proceed — which is the half a reader
// actually needs to trust, because each refusal is a credential NOT being minted.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenInClusterStoreNeedsClientTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENBAO_CA_FILE", filepath.Join(dir, "absent-ca.crt"))
	t.Setenv("OPENBAO_CLIENT_CERT_FILE", filepath.Join(dir, "absent.crt"))
	t.Setenv("OPENBAO_CLIENT_KEY_FILE", filepath.Join(dir, "absent.key"))
	prev := InClusterHTTPClient
	InClusterHTTPClient = NewInClusterHTTPClient()
	t.Cleanup(func() { InClusterHTTPClient = prev })

	if _, err := OpenInClusterStore(context.Background(), "harbor-provisioner"); err == nil {
		t.Fatal("must refuse without mounted client TLS — mTLS is the whole point of the mount")
	}
}
