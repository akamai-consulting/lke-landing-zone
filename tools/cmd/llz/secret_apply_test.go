package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenericSecretManifest(t *testing.T) {
	m := genericSecretManifest("ns1", "sec1", "secretId", "v@lue:with\nnewline")
	for _, want := range []string{
		"kind: Secret",
		"name: sec1",
		"namespace: ns1",
		"type: Opaque",
		"secretId: " + base64.StdEncoding.EncodeToString([]byte("v@lue:with\nnewline")),
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}
