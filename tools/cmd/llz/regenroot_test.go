package main

import "testing"

func TestParseBaoStatus(t *testing.T) {
	sealed, th := parseBaoStatus(`{"sealed":false,"t":3,"n":5}`)
	if sealed || th != 3 {
		t.Errorf("got sealed=%v t=%d, want false 3", sealed, th)
	}
	if s, _ := parseBaoStatus(`{"sealed":true,"t":2}`); !s {
		t.Error("want sealed=true")
	}
}

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

func TestParseTokenAndPolicies(t *testing.T) {
	if parseTokenField(`{"token":"s.deadbeef"}`) != "s.deadbeef" {
		t.Error("token parse")
	}
	if !policiesIncludeRoot(`{"data":{"policies":["default","root"]}}`) {
		t.Error("should include root")
	}
	if policiesIncludeRoot(`{"data":{"policies":["default"]}}`) {
		t.Error("should not include root")
	}
}

func TestSecretListed(t *testing.T) {
	names := []string{"OPENBAO_ROOT_TOKEN", "OTHER_SECRET"}
	if !secretListed(names, "OPENBAO_ROOT_TOKEN") {
		t.Error("should find OPENBAO_ROOT_TOKEN")
	}
	if secretListed(names, "MISSING") {
		t.Error("should not find MISSING")
	}
}
