package cli

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
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

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty(all empty) = %q, want empty", got)
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
		if got := templateid.NormalizeTemplateRepo(in); got != want {
			t.Errorf("templateid.NormalizeTemplateRepo(%q) = %q, want %q", in, got, want)
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

func TestSha256Hex(t *testing.T) {
	// Known vectors.
	if got := s3sig.SHA256Hex(""); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("s3sig.SHA256Hex(empty) = %q", got)
	}
	if got := s3sig.SHA256Hex("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("s3sig.SHA256Hex(abc) = %q", got)
	}
}

func TestSetHCLField(t *testing.T) {
	content := "foo = \"old\"\nbar = 1\n"
	got := tfvars.SetField(content, "foo", "\"new\"")
	if !strings.Contains(got, "foo = \"new\"") {
		t.Errorf("tfvars.SetField did not replace foo: %q", got)
	}
	if !strings.Contains(got, "bar = 1") {
		t.Errorf("tfvars.SetField clobbered bar: %q", got)
	}
}
