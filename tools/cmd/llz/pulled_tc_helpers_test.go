package main

import (
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatecommit"
)

// Helpers the moved tests use, copied across the boundary.

// writeInstanceDir runs the test in a fresh directory containing files
// (name → content) — chdirTemp plus the instance files the case needs.
// stubTemplateCommit replaces the tag→commit round-trip for the duration of a
// test. Every test in this file installs one: without it a non-SHA ref would send
// a real request to api.github.com, which is both slow and a hermeticity break.
func stubTemplateCommit(t *testing.T, fn func(repo, ref string) (string, bool)) {
	t.Helper()
	prev := templatecommit.Resolve
	t.Cleanup(func() { templatecommit.Resolve = prev })
	templatecommit.Resolve = fn
}

func writeInstanceDir(t *testing.T, files map[string]string) {
	t.Helper()
	chdirTemp(t)
	for name, body := range files {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// stubImagePublished replaces the registry round-trip for the duration of a test.
// It swaps templatecommit.ImagePublished — an EXPORTED seam, which is correct: the
// check moved packages but the test that needs to stub it did not.
func stubImagePublished(t *testing.T, fn func(image string) (bool, bool)) {
	t.Helper()
	prev := templatecommit.ImagePublished
	templatecommit.ImagePublished = fn
	t.Cleanup(func() { templatecommit.ImagePublished = prev })
}

// pinnedAt puts the test in an instance pinned at ref whose images all resolve.
func pinnedAt(t *testing.T, ref string) {
	t.Helper()
	writeInstanceDir(t, map[string]string{
		".copier-answers.yml": "_src_path: gh:acme/tmpl\nllz_version: " + ref + "\n",
	})
	stubTemplateCommit(t, func(string, string) (string, bool) { return repinSHA, true })
	stubImagePublished(t, func(string) (bool, bool) { return true, true })
}

const repinSHA = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"
