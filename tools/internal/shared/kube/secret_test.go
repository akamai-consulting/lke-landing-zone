package kube

// secret_test.go — SecretField's tests, which travelled with it.
//
// They were in cmd/llz/ci_assert_obj_certs_db_test.go and would have followed the
// helper into internal/assertobjstore by default. Leaving them there would have
// dropped this package below its coverage floor while the assertion's number rose
// for code it does not own — the per-package floors catch exactly that, which is
// what they are for.

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSecretField(t *testing.T) {
	raw := objSecretJSON("AK", "SK")
	if v, err := SecretField(raw, "AWS_ACCESS_KEY_ID"); err != nil || v != "AK" {
		t.Errorf("unexpected (%q,%v)", v, err)
	}
	if _, err := SecretField(raw, "NOPE"); err == nil {
		t.Error("a missing key must be an error — ESO not having materialized it is a finding")
	}
	empty, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID": base64.StdEncoding.EncodeToString([]byte("  ")),
	}})
	if _, err := SecretField(empty, "AWS_ACCESS_KEY_ID"); err == nil {
		t.Error("an empty credential must be an error, not an empty string handed to the signer")
	}
	if _, err := SecretField([]byte(`nope`), "x"); err == nil {
		t.Error("an unparseable Secret must be an error")
	}
}

func objSecretJSON(access, secret string) []byte {
	b, _ := json.Marshal(map[string]any{"data": map[string]string{
		"AWS_ACCESS_KEY_ID":     base64.StdEncoding.EncodeToString([]byte(access)),
		"AWS_SECRET_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(secret)),
	}})
	return b
}
