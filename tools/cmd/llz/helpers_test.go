package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestContainsString(t *testing.T) {
	ss := []string{"a", "b", "c"}
	if !containsString(ss, "b") {
		t.Error("containsString should find b")
	}
	if containsString(ss, "z") {
		t.Error("containsString should not find z")
	}
	if containsString(nil, "a") {
		t.Error("containsString(nil) should be false")
	}
}

func TestClusterFromEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://us-east-1.linodeobjects.com": "us-east-1",
		"http://nl-ams-1.linodeobjects.com":   "nl-ams-1",
		"us-east-1.linodeobjects.com":         "us-east-1",
		"https://example.com":                 "",
		"":                                    "",
	}
	for in, want := range cases {
		if got := clusterFromEndpoint(in); got != want {
			t.Errorf("clusterFromEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdminFlagAndBanner(t *testing.T) {
	if adminFlag(false) != "" {
		t.Error("adminFlag(false) should be empty")
	}
	if adminFlag(true) != " --admin" {
		t.Errorf("adminFlag(true) = %q, want ' --admin'", adminFlag(true))
	}
	if adminBanner(false) != "" {
		t.Error("adminBanner(false) should be empty")
	}
	if !strings.Contains(adminBanner(true), "[ADMIN:") {
		t.Errorf("adminBanner(true) = %q, want it to contain [ADMIN:", adminBanner(true))
	}
}

func TestFirst3(t *testing.T) {
	cases := map[string]string{"abcdef": "abc", "ab": "ab", "": "", "abc": "abc"}
	for in, want := range cases {
		if got := first3(in); got != want {
			t.Errorf("first3(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty(all empty) = %q, want empty", got)
	}
}

func TestGithubSlug(t *testing.T) {
	cases := map[string]string{
		"git@github.com:owner/repo.git":     "owner/repo",
		"https://github.com/owner/repo.git": "owner/repo",
		"https://github.com/owner/repo":     "owner/repo",
		"owner/repo.git":                    "owner/repo",
		"https://gitlab.com/owner/repo":     "", // other host
		"git@gitlab.com:owner/repo.git":     "", // other host
		"justaword":                         "",
	}
	for in, want := range cases {
		if got := githubSlug(in); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGhFineGrainedDispatchURL(t *testing.T) {
	u, err := url.Parse(ghFineGrainedDispatchURL("llz-e2e-dispatch", "my-org"))
	if err != nil {
		t.Fatalf("not a valid URL: %v", err)
	}
	if u.Host != "github.com" || u.Path != "/settings/personal-access-tokens/new" {
		t.Errorf("unexpected host/path: %q", u)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"name": "llz-e2e-dispatch", "target_name": "my-org", "expires_in": "90",
		"contents": "write", "actions": "write", "workflows": "write",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	// owner omitted -> no target_name.
	if q2 := mustQuery(t, ghFineGrainedDispatchURL("n", "")); q2.Has("target_name") {
		t.Errorf("empty owner should omit target_name, got %v", q2)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestIndent(t *testing.T) {
	if got := indent("a\nb", "  "); got != "  a\n  b" {
		t.Errorf("indent = %q, want '  a\\n  b'", got)
	}
	// Trailing newline is trimmed before indenting.
	if got := indent("x\n", ">"); got != ">x" {
		t.Errorf("indent(trailing nl) = %q, want '>x'", got)
	}
}

func TestNormalizeTemplateRepo(t *testing.T) {
	cases := map[string]string{
		"  ":                                "",
		"gh:owner/repo.git":                 "owner/repo",
		"git@github.com:owner/repo.git":     "owner/repo",
		"https://github.com/owner/repo.git": "owner/repo",
	}
	for in, want := range cases {
		if got := normalizeTemplateRepo(in); got != want {
			t.Errorf("normalizeTemplateRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrHelpers(t *testing.T) {
	if orAll("") != "(all)" || orAll("x") != "x" {
		t.Error("orAll wrong")
	}
	if !strings.HasPrefix(orNone(""), "(none") || orNone("x") != "x" {
		t.Error("orNone wrong")
	}
}

func TestQuote(t *testing.T) {
	if got := quote("x"); got != `"x"` {
		t.Errorf("quote = %q, want \"x\"", got)
	}
}

func TestSha256Hex(t *testing.T) {
	// Known vectors.
	if got := sha256Hex(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("sha256Hex(empty) = %q", got)
	}
	if got := sha256Hex("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256Hex(abc) = %q", got)
	}
}

func TestSetHCLField(t *testing.T) {
	content := "foo = \"old\"\nbar = 1\n"
	got := setHCLField(content, "foo", "\"new\"")
	if !strings.Contains(got, "foo = \"new\"") {
		t.Errorf("setHCLField did not replace foo: %q", got)
	}
	if !strings.Contains(got, "bar = 1") {
		t.Errorf("setHCLField clobbered bar: %q", got)
	}
}
