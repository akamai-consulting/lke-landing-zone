package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// stubTemplateCommit replaces the tag→commit round-trip for the duration of a
// test. Every test in this file installs one: without it a non-SHA ref would send
// a real request to api.github.com, which is both slow and a hermeticity break.
func stubTemplateCommit(t *testing.T, fn func(repo, ref string) (string, bool)) {
	t.Helper()
	prev := resolveTemplateCommit
	t.Cleanup(func() { resolveTemplateCommit = prev })
	resolveTemplateCommit = fn
}

func TestRunAssertImageFresh(t *testing.T) {
	const sha = "0d634d7d54a138314be21d0891c376fbae99519a"
	const other = "7ec07dc7929384cf393bbde98002d7089097e673"
	// Nothing resolves: the guard must behave exactly as it did before the tag
	// resolution existed, i.e. warn + pass rather than fail on an unanswered question.
	unresolvable := func(string, string) (string, bool) { return "", false }
	cases := []struct {
		name, baked, ref string
		resolve          func(repo, ref string) (string, bool)
		wantErr          string // substring; "" means no error
	}{
		{"dev matches full sha", "dev-" + sha, sha, unresolvable, ""},
		{"dev matches short ref", "dev-" + sha, sha[:12], unresolvable, ""},
		{"dev short build matches full ref", "dev-" + sha[:12], sha, unresolvable, ""},
		{"dev sha mismatch", "dev-" + sha, other, unresolvable, "image/template skew"},
		{"dev vs unresolvable ref skips", "dev-" + sha, "main", unresolvable, ""},
		{"unstamped dev skips", "dev", sha, unresolvable, ""},
		{"empty version skips", "", sha, unresolvable, ""},
		{"release tag matches", "v1.2.3", "v1.2.3", unresolvable, ""},
		{"release tag mismatch", "v1.2.3", "v1.2.4", unresolvable, "image/template skew"},
		{"release vs sha skips", "v1.2.3", sha, unresolvable, ""},
		{"unresolvable ref errors", "dev-" + sha, "", unresolvable, "cannot resolve the template ref"},

		// The case this guard used to skip — a release-tag pin against a dev image.
		// It is the DEFAULT shape of a fresh instance, so both outcomes must be right.
		{"tag resolving to the baked commit passes", "dev-" + sha, "v0.0.39",
			func(string, string) (string, bool) { return sha, true }, ""},
		{"tag resolving to a different commit fails", "dev-" + sha, "v0.0.39",
			func(string, string) (string, bool) { return other, true }, "image/template skew"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubTemplateCommit(t, tc.resolve)
			err := runAssertImageFresh(tc.baked, tc.ref, "acme/tmpl")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("runAssertImageFresh(%q,%q) = %v, want nil", tc.baked, tc.ref, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runAssertImageFresh(%q,%q) = %v, want error containing %q", tc.baked, tc.ref, err, tc.wantErr)
			}
		})
	}
}

// The message is the whole product here: the operator who hits this has just been
// told by `llz render --check` to run a command that cannot help them. It has to
// carry the resolved commit (so the tag and the sha can be reconciled by eye) and
// the exact re-pin, or they are left guessing again.
func TestAssertImageFreshSkewMessageIsActionable(t *testing.T) {
	const baked = "0d634d7d54a138314be21d0891c376fbae99519a"
	const pin = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
	stubTemplateCommit(t, func(string, string) (string, bool) { return pin, true })

	err := runAssertImageFresh("dev-"+baked, "v0.0.39", "acme/tmpl")
	if err == nil {
		t.Fatal("want skew error")
	}
	for _, want := range []string{
		"v0.0.39 (= " + pin + ")", // the tag AND what it resolves to
		"gh variable set TF_IMAGE",
		"gh variable set KUBE_IMAGE",
		"ci-tofu:sha-" + pin,
		"ci-kubernetes:sha-" + pin,
		"llz render --check", // names the symptom they actually saw
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("skew message missing %q:\n%v", want, err)
		}
	}
}

func TestAssertImageFreshCmdWiring(t *testing.T) {
	c := ciAssertImageFreshCmd()
	if c.Use != "assert-image-fresh" {
		t.Errorf("Use = %q", c.Use)
	}
	// The flag survives as an escape hatch, but must NOT be required: the workflow
	// now runs the verb bare and the ref comes from the instance's own pin.
	f := c.Flags().Lookup("template-ref")
	if f == nil {
		t.Fatal("missing --template-ref override flag")
	}
	if a := f.Annotations[cobra.BashCompOneRequiredFlag]; len(a) > 0 && a[0] == "true" {
		t.Error("--template-ref must not be required — the pin is resolved from the instance")
	}
}
