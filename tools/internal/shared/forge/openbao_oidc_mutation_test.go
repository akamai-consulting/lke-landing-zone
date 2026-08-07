package forge

import (
	"encoding/json"
	"strings"
	"testing"
)

// The owner is the leading path segment of the slug, and it is what the GitHub
// audience is built from. A malformed slug with no owner segment ("/proj") must
// not yield an EMPTY owner: bound_audiences would become the bare, owner-less
// "https://github.com/", an audience no longer pinned to any owner. The split
// therefore requires the separator to be *past* index 0.
func TestOpenBaoJWTRoleBody_SlugWithoutOwnerSegment(t *testing.T) {
	f, _ := New(GitHub, "")
	body, err := OpenBaoJWTRoleBody(f, "/proj", "platform-ci")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		BoundAudiences []string          `json:"bound_audiences"`
		BoundClaims    map[string]string `json:"bound_claims"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.BoundAudiences) != 1 {
		t.Fatalf("bound_audiences = %v, want exactly one entry", got.BoundAudiences)
	}
	if aud := got.BoundAudiences[0]; aud == "https://github.com/" || strings.HasSuffix(aud, "/") {
		t.Errorf("bound_audiences = %q — an owner-less slug must not collapse the audience to the bare owner prefix", aud)
	}
	// The repo claim is unaffected by the owner split and still pins the full slug.
	if got.BoundClaims["repository"] != "/proj" {
		t.Errorf("bound_claims = %v, want the full slug in `repository`", got.BoundClaims)
	}
}

// The ordinary shape, for contrast: the owner is everything before the first '/'
// even when the name itself contains further separators.
func TestOpenBaoJWTRoleBody_OwnerIsFirstSegment(t *testing.T) {
	f, _ := New(GitHub, "")
	body, _ := OpenBaoJWTRoleBody(f, "acme/platform", "platform-ci")
	if !strings.Contains(body, `"bound_audiences":["https://github.com/acme"]`) {
		t.Errorf("body = %s, want the owner-scoped audience", body)
	}
	// No separator at all: the whole slug is the owner (the pre-existing behavior
	// the `i > 0` guard also produces for a leading separator).
	body, _ = OpenBaoJWTRoleBody(f, "solo", "platform-ci")
	if !strings.Contains(body, `"bound_audiences":["https://github.com/solo"]`) {
		t.Errorf("body = %s, want the whole slug as owner when it has no '/'", body)
	}
}
