package forge

import "testing"

func TestLaneName(t *testing.T) {
	cases := []struct {
		flavor Flavor
		want   string
	}{
		{GitHub, "e2e"}, // unchanged — no migration for the existing lane
		{GHEC, "e2e-ghec"},
		{GHES, "e2e-ghes"},
		{GitLab, "e2e-gitlab"},
	}
	for _, c := range cases {
		if got := LaneName(c.flavor, "e2e"); got != c.want {
			t.Errorf("LaneName(%s, e2e) = %q, want %q", c.flavor, got, c.want)
		}
	}
}

// The whole point: two forge lanes on one base must not collide.
func TestLaneName_LanesAreDistinct(t *testing.T) {
	seen := map[string]Flavor{}
	for _, f := range []Flavor{GitHub, GHEC, GHES, GitLab} {
		name := LaneName(f, "e2e")
		if prev, dup := seen[name]; dup {
			t.Errorf("lane collision: %s and %s both produce %q", prev, f, name)
		}
		seen[name] = f
	}
}
