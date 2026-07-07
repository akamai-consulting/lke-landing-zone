package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	mustWrite(t, src, "hello")
	dst := filepath.Join(dir, "nested", "deep", "dst.txt") // parent dirs created
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" {
		t.Errorf("copyFile dst = %q, want hello", b)
	}
	if err := copyFile(filepath.Join(dir, "missing"), dst); err == nil {
		t.Error("copyFile(missing src) = nil, want error")
	}
}

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "a")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "sub", "b.txt"), "b")

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	got := walkFilesRel(dst)
	if len(got) != 2 || !containsString(got, "a.txt") || !containsString(got, filepath.Join("sub", "b.txt")) {
		t.Errorf("copyTree produced %v, want a.txt and sub/b.txt", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); string(b) != "b" {
		t.Errorf("copied sub/b.txt = %q, want b", b)
	}
}

func TestWalkFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "z.txt"), "")
	mustWrite(t, filepath.Join(dir, "a.txt"), "")
	files := walkFiles(dir)
	if len(files) != 2 {
		t.Fatalf("walkFiles returned %d, want 2", len(files))
	}
	// Sorted: a before z.
	if filepath.Base(files[0]) != "a.txt" || filepath.Base(files[1]) != "z.txt" {
		t.Errorf("walkFiles not sorted: %v", files)
	}
	rel := walkFilesRel(dir)
	if !containsString(rel, "a.txt") || filepath.IsAbs(rel[0]) {
		t.Errorf("walkFilesRel = %v, want relative paths", rel)
	}
}

func TestGrepToken(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "hit.yaml"), "key: value\nsecret: SENTINEL_TOKEN\n")
	mustWrite(t, filepath.Join(dir, "miss.yaml"), "nothing here\n")
	extra := filepath.Join(t.TempDir(), "extra.txt")
	mustWrite(t, extra, "also has SENTINEL_TOKEN inline")

	hits := grepToken("SENTINEL_TOKEN", dir, []string{extra})
	if len(hits) != 2 {
		t.Fatalf("grepToken found %d hits, want 2: %v", len(hits), hits)
	}
	// Hits carry file:line: form; the overlay match is on line 2.
	if !containsSub(hits, "hit.yaml:2:") {
		t.Errorf("grepToken hits missing hit.yaml:2: — %v", hits)
	}
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	mustWrite(t, p, "lower")
	if err := editFile(p, strings.ToUpper); err != nil {
		t.Fatalf("editFile: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "LOWER" {
		t.Errorf("editFile result = %q, want LOWER", b)
	}
	if err := editFile(filepath.Join(dir, "missing"), strings.ToUpper); err == nil {
		t.Error("editFile(missing) = nil, want error")
	}
}

func TestStampTemplateVersion(t *testing.T) {
	chdirTemp(t)
	// No .copier-answers.yml, and the remotes resolve to nothing, so the repo
	// falls back to the default; HEAD/describe come from the stub.
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

	captureStdout(t, func() {
		if err := stampTemplateVersion("dev"); err != nil {
			t.Fatalf("stampTemplateVersion: %v", err)
		}
	})

	b, err := os.ReadFile(".template-version")
	if err != nil {
		t.Fatalf("read .template-version: %v", err)
	}
	var tv templateVersion
	if err := json.Unmarshal(b, &tv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tv.Env != "dev" || tv.Schema != 1 || tv.Generator != "llz" {
		t.Errorf("stamped meta wrong: %+v", tv)
	}
	if tv.TemplateRepo != defaultTemplateRepo {
		t.Errorf("TemplateRepo = %q, want default %q", tv.TemplateRepo, defaultTemplateRepo)
	}
	if tv.TemplateSHA != "deadbeefcafe1234" {
		t.Errorf("TemplateSHA = %q, want the stubbed HEAD", tv.TemplateSHA)
	}
}

func TestStampTemplateVersionWithOptions(t *testing.T) {
	chdirTemp(t)
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return []byte("unexpected-git-call\n"), nil
	})

	captureStdout(t, func() {
		if err := stampTemplateVersionWithOptions(stampTemplateVersionOptions{
			Repo: "https://github.com/akamai-consulting/lke-landing-zone.git",
			Ref:  "feature/ref",
			SHA:  "1234567890abcdef",
			Env:  "e2e",
			Now:  "2026-07-07T12:00:00Z",
		}); err != nil {
			t.Fatalf("stampTemplateVersionWithOptions: %v", err)
		}
	})

	b, err := os.ReadFile(".template-version")
	if err != nil {
		t.Fatalf("read .template-version: %v", err)
	}
	var tv templateVersion
	if err := json.Unmarshal(b, &tv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tv.TemplateRepo != defaultTemplateRepo {
		t.Errorf("TemplateRepo = %q, want %q", tv.TemplateRepo, defaultTemplateRepo)
	}
	if tv.TemplateRef != "feature/ref" || tv.TemplateSHA != "1234567890abcdef" || tv.Env != "e2e" || tv.StampedAt != "2026-07-07T12:00:00Z" {
		t.Errorf("stamped options not honored: %+v", tv)
	}
}

func TestStampTemplateVersionCommandWiring(t *testing.T) {
	c := ciStampTemplateVersionCmd()
	for _, flag := range []string{"repo", "ref", "sha", "env", "now"} {
		if c.Flags().Lookup(flag) == nil {
			t.Fatalf("missing --%s flag", flag)
		}
	}
	if err := c.Args(c, []string{"extra"}); err == nil {
		t.Fatal("stamp-template-version accepted positional args")
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
