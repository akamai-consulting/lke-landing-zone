package ghcli_test

// The two functions here with a decision in them are Quote and SecretSetArgv.
// Both moved from cmd/llz with tests that stayed behind in commands_test.go,
// which still exercises them through their new names — but this package carries
// its own now, because a display helper whose output a human is meant to paste
// into a shell is exactly the thing that must not silently start producing
// something unpasteable.

import (
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
)

func TestQuoteOnlyQuotesWhatAShellWouldMangle(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		argv       []string
	}{
		{"plain words are left alone", "gh secret set NAME", []string{"gh", "secret", "set", "NAME"}},
		{"a space forces quoting", `echo 'a b'`, []string{"echo", "a b"}},
		{"the empty string must survive", `echo ''`, []string{"echo", ""}},
		{"shell metacharacters force quoting", `echo 'a|b'`, []string{"echo", "a|b"}},
		// SINGLE quotes, and that is the point: double quotes would still let the
		// shell expand $VAR and backticks in a value llz is only trying to DISPLAY.
		{"a dollar sign must not stay expandable", `echo '$HOME'`, []string{"echo", "$HOME"}},
		{"an embedded single quote is escaped, not dropped", `echo 'it'\''s'`, []string{"echo", "it's"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghcli.Quote(tc.argv); got != tc.want {
				t.Errorf("Quote(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

func TestHostPrefersGHHost(t *testing.T) {
	t.Setenv("GH_HOST", "  ghe.example.com  ")
	if got := ghcli.Host(); got != "ghe.example.com" {
		t.Errorf("Host() = %q, want the trimmed GH_HOST — GHES instances are the reason this exists", got)
	}
	t.Setenv("GH_HOST", "   ")
	if got := ghcli.Host(); got != "github.com" {
		t.Errorf("whitespace-only GH_HOST must fall back to github.com, got %q", got)
	}
	_ = os.Unsetenv("GH_HOST")
}

func TestVariableSetArgvIsNeverEnvScoped(t *testing.T) {
	// Variables are repo-level in this tree; only secrets carry --env. If that
	// ever changes, it changes here and not by accident.
	got := strings.Join(ghcli.VariableSetArgv("TEMPLATE_REF"), " ")
	if got != "gh variable set TEMPLATE_REF" {
		t.Errorf("VariableSetArgv = %q", got)
	}
}
