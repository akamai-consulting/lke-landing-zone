package openbao

import (
	"testing"
)

func TestParseIsSelf(t *testing.T) {
	if !parseIsSelf(`{"is_self":true}`) {
		t.Error("want is_self true")
	}
	if parseIsSelf(`{"is_self":false,"ha_mode":null}`) {
		t.Error("want is_self false")
	}
	if parseIsSelf(`not json`) {
		t.Error("bad json should be false")
	}
}

func TestParseGenRootInitAndStep(t *testing.T) {
	n, otp := parseGenRootInit(`{"nonce":"abc","otp":"xyz"}`)
	if n != "abc" || otp != "xyz" {
		t.Errorf("init: got %q %q", n, otp)
	}
	complete, p, r, enc := parseGenRootStep(`{"complete":true,"progress":3,"required":3,"encoded_token":"ENC"}`)
	if !complete || p != 3 || r != 3 || enc != "ENC" {
		t.Errorf("step: got %v %d %d %q", complete, p, r, enc)
	}
	complete2, p2, _, enc2 := parseGenRootStep(`{"complete":false,"progress":1,"required":3,"encoded_token":""}`)
	if complete2 || p2 != 1 || enc2 != "" {
		t.Errorf("partial step misparsed: %v %d %q", complete2, p2, enc2)
	}
}

func TestParseTokenField(t *testing.T) {
	if parseTokenField(`{"token":"s.deadbeef"}`) != "s.deadbeef" {
		t.Error("token parse")
	}
}

func TestSecretListed(t *testing.T) {
	out := "NAME                    UPDATED\nOPENBAO_ROOT_TOKEN      2026-06-10\nOTHER_SECRET            2026-06-09\n"
	if !secretListed(out, "OPENBAO_ROOT_TOKEN") {
		t.Error("should find OPENBAO_ROOT_TOKEN")
	}
	if secretListed(out, "MISSING") {
		t.Error("should not find MISSING")
	}
}
