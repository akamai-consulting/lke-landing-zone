package configreadiness

// The five tests here all drive this package's own state model — LoadEnvFiles,
// E2ERequirements, GHSecretNames and ghVars. ghVars in particular returns an
// unexported []ghVar, so its test is only writable in-package.
//
// They reached package main by an over-greedy extraction regex, three times in one
// session. Splitting by LINE RANGE off the parsed function boundaries, as this was
// finally done, is the mitigation: a non-greedy match across a file of same-shaped
// functions silently takes the wrong span.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFiles(t *testing.T) {
	chdirTemp(t)
	if err := os.Mkdir(".llz", 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".llz", "secrets.env"), "A=1\n# a comment\n\nB=two\n")
	mustWrite(t, filepath.Join(".llz", "vars.env"), "C=3\n")

	secrets, vars := LoadEnvFiles()
	if secrets["A"] != "1" || secrets["B"] != "two" {
		t.Errorf("secrets = %v, want A=1 B=two", secrets)
	}
	if len(secrets) != 2 {
		t.Errorf("secrets has %d entries, want 2 (comment/blank skipped)", len(secrets))
	}
	if vars["C"] != "3" {
		t.Errorf("vars = %v, want C=3", vars)
	}
}

func TestLoadEnvFilesMissing(t *testing.T) {
	chdirTemp(t) // no .llz dir
	secrets, vars := LoadEnvFiles()
	if secrets == nil || vars == nil {
		t.Error("LoadEnvFiles returned nil maps; want empty non-nil")
	}
	if len(secrets) != 0 || len(vars) != 0 {
		t.Errorf("LoadEnvFiles(missing) = (%v, %v), want empty", secrets, vars)
	}
}

func TestE2ERequirements(t *testing.T) {
	base := E2ERequirements(false)
	if len(base) == 0 {
		t.Fatal("E2ERequirements(false) is empty")
	}
	found := false
	for _, r := range base {
		if r.Name == "LINODE_API_TOKEN" {
			found = true
		}
	}
	if !found {
		t.Error("E2ERequirements missing LINODE_API_TOKEN")
	}
	// admin adds the template-repo e2e-harness entries on top.
	if admin := E2ERequirements(true); len(admin) < len(base) {
		t.Errorf("admin reqs (%d) < base reqs (%d), want >=", len(admin), len(base))
	}
}

func TestGhSecretNames(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Errorf("GHSecretNames shelled out to %q, want gh", name)
		}
		return []byte(`{"secrets":[{"name":"TOKEN_A"},{"name":"TOKEN_B"}]}`), nil
	})
	names := GHSecretNames("repos/o/r/actions/secrets")
	if len(names) != 2 || !containsString(names, "TOKEN_A") || !containsString(names, "TOKEN_B") {
		t.Errorf("GHSecretNames = %v, want [TOKEN_A TOKEN_B]", names)
	}

	// gh failure -> empty (ghAPI returns nil, unmarshal of nil is a no-op).
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, os.ErrNotExist })
	if got := GHSecretNames("x"); len(got) != 0 {
		t.Errorf("GHSecretNames(gh error) = %v, want empty", got)
	}
}

func TestGhVars(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`{"variables":[{"name":"TF_IMAGE","value":"ghcr.io/x"}]}`), nil
	})
	vars := ghVars("repos/o/r/actions/variables")
	if len(vars) != 1 || vars[0].Name != "TF_IMAGE" || vars[0].Value != "ghcr.io/x" {
		t.Errorf("ghVars = %+v, want one TF_IMAGE var", vars)
	}
}

// containsString: the definition travelled out of package main with a file this
// extraction moved, leaving both sides using it. Defined here rather than hunted
// for — it is three lines and slices.Contains-shaped.
func containsString(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}
