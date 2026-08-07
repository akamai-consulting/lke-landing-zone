package harborauth

// harborauth_test.go — the parsing half, moved with the client. The bearer
// challenge and the token's scope claims are pure string work, and they are where
// a registry gets refused for a reason that looks like a credential problem and is
// not.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeRobotSecret(t *testing.T) {
	c, err := DecodeRobotSecret(robotSecretJSON("robot$ci", "s3cret", "harbor.example.com"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Username != "robot$ci" || c.Password != "s3cret" || c.RegistryHost != "harbor.example.com" {
		t.Errorf("unexpected creds %+v", c)
	}

	// THE regression: "harbor." is NON-EMPTY, so every `== ""` guard passes it,
	// including the systeminfo fallback that was supposed to rescue this case.
	_, err = DecodeRobotSecret(robotSecretJSON("robot$ci", "s3cret", "harbor."))
	if err == nil {
		t.Fatal(`registry_host "harbor." must be rejected — it is non-empty and every push and pull 401s`)
	}
	if !strings.Contains(err.Error(), "truncation") {
		t.Errorf("the failure should name the truncation class, got %q", err)
	}

	// Missing keys mean ESO has not materialized the Secret — a distinct failure
	// from a bad host, and it must not be read as an empty credential.
	partial, _ := json.Marshal(map[string]any{"data": map[string]string{"username": robotB64("x")}})
	if _, err := DecodeRobotSecret(partial); err == nil {
		t.Error("a Secret missing password/registry_host must fail")
	}
	if _, err := DecodeRobotSecret([]byte(`nope`)); err == nil {
		t.Error("an unparseable Secret must fail")
	}
	bad, _ := json.Marshal(map[string]any{"data": map[string]string{
		"username": "!!!not base64!!!", "password": robotB64("p"), "registry_host": robotB64("h.example.com")}})
	if _, err := DecodeRobotSecret(bad); err == nil {
		t.Error("non-base64 Secret data must fail")
	}
}

func TestParseBearerChallenge(t *testing.T) {
	c, err := ParseBearerChallenge(`Bearer realm="https://h.example.com/service/token",service="harbor-registry"`)
	if err != nil || c.Realm != "https://h.example.com/service/token" || c.Service != "harbor-registry" {
		t.Fatalf("unexpected (%+v,%v)", c, err)
	}
	if _, err := ParseBearerChallenge(`Basic realm="x"`); err == nil {
		t.Error("a non-Bearer challenge must fail")
	}
	if _, err := ParseBearerChallenge(`Bearer service="x"`); err == nil {
		t.Error("a challenge with no realm must fail — there is nowhere to get a token")
	}
}

func TestParseTokenResponseAndGrantedActions(t *testing.T) {
	raw := []byte(`{"token":"abc","access":[{"type":"repository","name":"library/x","actions":["pull","push"]}]}`)
	tok, access, err := ParseTokenResponse(raw)
	if err != nil || tok != "abc" {
		t.Fatalf("unexpected (%q,%v)", tok, err)
	}
	g := GrantedActions(access, "library/x")
	if !g["pull"] || !g["push"] {
		t.Errorf("expected pull+push, got %v", g)
	}
	// Access for a DIFFERENT repo must not count.
	if len(GrantedActions(access, "library/other")) != 0 {
		t.Error("access scoped to another repository must not be read as a grant")
	}
	if _, _, err := ParseTokenResponse([]byte(`{"access":[]}`)); err == nil {
		t.Error("a response with no token must fail")
	}
	if got := MissingActions(map[string]bool{"pull": true}, "pull", "push"); len(got) != 1 || got[0] != "push" {
		t.Errorf("MissingActions should report exactly the absent action, got %v", got)
	}
}

// Fixture builders, duplicated on this side: a test helper cannot cross a package
// boundary, and both files still need one.
func robotB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func robotSecretJSON(user, pass, host string) []byte {
	obj := map[string]any{"data": map[string]string{
		"username": robotB64(user), "password": robotB64(pass), "registry_host": robotB64(host),
	}}
	b, _ := json.Marshal(obj)
	return b
}
