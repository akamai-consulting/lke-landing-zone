package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/sustain"
)

func TestValidateOBJCluster(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},         // empty is allowed (caller decides)
		{"us-sea-1", false}, // legacy cluster
		{"us-ord-1", false}, // legacy cluster
		{"ap-south-1", false},
		{"us-iad-1", false},
		{"us-sea-2", false},  // newer-generation cluster — valid
		{"us-ord-10", false}, // newer-generation cluster — valid
		{"us-east-12", false},
		{"us-sea", true},    // not a cluster id (no datacenter ordinal)
		{"ussea1", true},    // not a cluster id
		{"0.0.0.0/0", true}, // a CIDR, not a cluster id
		{"us-sea-1 ", true}, // trailing space → not a match
		{"US-SEA-1", true},  // uppercase → not a match
	}
	for _, c := range cases {
		err := validateOBJCluster(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validateOBJCluster(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

// Provenance is DERIVED, never stamped to disk: with no .copier-answers.yml the
// repo falls back to the default and HEAD/describe come from git.
func TestResolveTemplateVersionFallsBackToGit(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "rev-parse"):
			return []byte("deadbeefcafe1234\n"), nil
		case strings.Contains(j, "describe"):
			return []byte("v1.2.3\n"), nil
		default: // remote get-url -> empty
			return []byte(""), nil
		}
	})

	tv := sustain.ResolveTemplateVersion(sustainDeps())
	if tv.Schema != 1 || tv.Generator != "llz" {
		t.Errorf("resolved meta wrong: %+v", tv)
	}
	if tv.TemplateRepo != sustain.DefaultTemplateRepo {
		t.Errorf("TemplateRepo = %q, want default %q", tv.TemplateRepo, sustain.DefaultTemplateRepo)
	}
	if tv.TemplateSHA != "deadbeefcafe1234" || tv.TemplateRef != "v1.2.3" {
		t.Errorf("git fallback not used: %+v", tv)
	}
	if _, err := os.Stat(".template-version"); !os.IsNotExist(err) {
		t.Errorf("resolving provenance must not write a stamp file; stat err = %v", err)
	}
}

// copier's answers are the authority when present — no git calls needed.
func TestResolveTemplateVersionFromAnswers(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return []byte("unexpected-git-call\n"), nil
	})
	mustWrite(t, ".copier-answers.yml", "_commit: 1234567890abcdef\n_src_path: gh:akamai-consulting/lke-landing-zone\nllz_version: v9.9.9\n")

	tv := sustain.ResolveTemplateVersion(sustainDeps())
	if tv.TemplateRepo != sustain.DefaultTemplateRepo {
		t.Errorf("TemplateRepo = %q, want %q", tv.TemplateRepo, sustain.DefaultTemplateRepo)
	}
	if tv.TemplateSHA != "1234567890abcdef" || tv.TemplateRef != "v9.9.9" {
		t.Errorf("answers not honored: %+v", tv)
	}
}

// A not-yet-upgraded instance still carrying the retired stamp keeps working:
// the legacy file fills what the answers cannot.
func TestResolveTemplateVersionFallsBackToLegacyStamp(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) { return []byte(""), nil })
	mustWrite(t, ".template-version", `{"schema":1,"template_repo":"myorg/lke-landing-zone","template_ref":"v0.0.27","template_sha":"abc1234567"}`)

	tv := sustain.ResolveTemplateVersion(sustainDeps())
	if tv.TemplateRepo != "myorg/lke-landing-zone" || tv.TemplateRef != "v0.0.27" || tv.TemplateSHA != "abc1234567" {
		t.Errorf("legacy stamp not honored: %+v", tv)
	}
}

func TestLoadEnvFiles(t *testing.T) {
	chdirTemp(t)
	if err := os.Mkdir(".llz", 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(".llz", "secrets.env"), "A=1\n# a comment\n\nB=two\n")
	mustWrite(t, filepath.Join(".llz", "vars.env"), "C=3\n")

	secrets, vars := loadEnvFiles()
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
	secrets, vars := loadEnvFiles()
	if secrets == nil || vars == nil {
		t.Error("loadEnvFiles returned nil maps; want empty non-nil")
	}
	if len(secrets) != 0 || len(vars) != 0 {
		t.Errorf("loadEnvFiles(missing) = (%v, %v), want empty", secrets, vars)
	}
}

func TestE2ERequirements(t *testing.T) {
	base := e2eRequirements(false)
	if len(base) == 0 {
		t.Fatal("e2eRequirements(false) is empty")
	}
	found := false
	for _, r := range base {
		if r.Name == "LINODE_API_TOKEN" {
			found = true
		}
	}
	if !found {
		t.Error("e2eRequirements missing LINODE_API_TOKEN")
	}
	// admin adds the template-repo e2e-harness entries on top.
	if admin := e2eRequirements(true); len(admin) < len(base) {
		t.Errorf("admin reqs (%d) < base reqs (%d), want >=", len(admin), len(base))
	}
}

func TestGhSecretNames(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" {
			t.Errorf("ghSecretNames shelled out to %q, want gh", name)
		}
		return []byte(`{"secrets":[{"name":"TOKEN_A"},{"name":"TOKEN_B"}]}`), nil
	})
	names := ghSecretNames("repos/o/r/actions/secrets")
	if len(names) != 2 || !containsString(names, "TOKEN_A") || !containsString(names, "TOKEN_B") {
		t.Errorf("ghSecretNames = %v, want [TOKEN_A TOKEN_B]", names)
	}

	// gh failure -> empty (ghAPI returns nil, unmarshal of nil is a no-op).
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, os.ErrNotExist })
	if got := ghSecretNames("x"); len(got) != 0 {
		t.Errorf("ghSecretNames(gh error) = %v, want empty", got)
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
