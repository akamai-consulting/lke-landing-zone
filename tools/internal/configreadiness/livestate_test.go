package configreadiness

// TestLiveStateValue had to move IN-PACKAGE: it constructs a LiveState from its
// unexported maps, which is the right way to test the env-wins-over-repo
// resolution and impossible from outside.

import "testing"

func TestLiveStateValue(t *testing.T) {
	s := LiveState{
		envVars:  map[string]string{"A": "env"},
		repoVars: map[string]string{"A": "repo", "B": "only-repo"},
	}
	if v := s.Value("A"); v != "env" { // env scope wins
		t.Errorf("value(A) = %q, want env", v)
	}
	if v := s.Value("B"); v != "only-repo" {
		t.Errorf("value(B) = %q, want only-repo", v)
	}
	if v := s.Value("missing"); v != "" {
		t.Errorf("value(missing) = %q, want empty", v)
	}
}
