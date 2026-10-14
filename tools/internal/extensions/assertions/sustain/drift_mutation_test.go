package sustain

import "testing"

// Moved with drift.go, which owns githubSlug.
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
