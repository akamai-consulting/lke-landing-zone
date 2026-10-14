package openbao

import "testing"

// Root wins when both are set (an operator mid-break-glass keeps root's reach);
// a team token alone is used as-is, so an operated spec.openbao mount can be
// administered without root.
func TestExecTokenPrefersRootThenTeam(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	t.Setenv("OPENBAO_TOKEN", "")
	if tok, _ := execToken(); tok != "" {
		t.Fatalf("no tokens set, got %q", tok)
	}
	t.Setenv("OPENBAO_TOKEN", "team")
	if tok, v := execToken(); tok != "team" || v != "OPENBAO_TOKEN" {
		t.Fatalf("team only: got %q from %s", tok, v)
	}
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	if tok, v := execToken(); tok != "root" || v != "OPENBAO_ROOT_TOKEN" {
		t.Fatalf("both set: got %q from %s", tok, v)
	}
}
