package kube

import (
	"encoding/base64"
	"strings"
	"testing"
)

// SecretManifest arrived here at 0% coverage: its tests were the peer-CA verb's,
// and they went to internal/baoca with the code that calls it. A pure renderer
// with no test of its own is exactly the shape that rots silently, so it gets one
// here rather than a lowered floor.
func TestSecretManifest(t *testing.T) {
	got := SecretManifest("llz-openbao", "openbao-peer-tls", "ca.crt", "-----BEGIN CERT-----\nx\n")

	for _, want := range []string{
		"kind: Secret",
		"name: openbao-peer-tls",
		"namespace: llz-openbao",
		"type: Opaque",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}

	// THE VALUE IS BASE64 SO NOTHING HAS TO BE YAML-ESCAPED. A PEM carries
	// newlines and a password may carry a colon or a leading `*`; both would need
	// quoting rules under `stringData:`, and getting those wrong writes a subtly
	// different secret rather than failing.
	enc := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERT-----\nx\n"))
	if !strings.Contains(got, "ca.crt: "+enc) {
		t.Errorf("value must be base64 under data:, got:\n%s", got)
	}
	if strings.Contains(got, "stringData") || strings.Contains(got, "BEGIN CERT") {
		t.Errorf("the raw value must not appear in the manifest:\n%s", got)
	}
}

// A key with dots is the common case (ca.crt, tls.key) and must not be mangled.
func TestSecretManifestKeepsDottedKeys(t *testing.T) {
	if got := SecretManifest("ns", "n", "tls.key", "v"); !strings.Contains(got, "tls.key: ") {
		t.Errorf("dotted key mangled:\n%s", got)
	}
}
