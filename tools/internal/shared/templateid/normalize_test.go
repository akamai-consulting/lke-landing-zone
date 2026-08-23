package templateid

// NormalizeTemplateRepo arrived from internal/extensions/sustain at 0% coverage:
// this package held only consts, so it had no test file at all.

import "testing"

func TestDefaultRepoIsDerived(t *testing.T) {
	// The point of deriving it: sustain held this same string written out by hand,
	// and a third copy of a fact is how two callers come to disagree.
	if DefaultRepo != DefaultOrg+"/"+Name {
		t.Errorf("DefaultRepo = %q, want %q", DefaultRepo, DefaultOrg+"/"+Name)
	}
}

func TestNormalizeTemplateRepo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"akamai-consulting/lke-landing-zone", "akamai-consulting/lke-landing-zone"},
		{"  spaced/repo  ", "spaced/repo"},
		{"gh:org/name", "org/name"},
		{"gh:org/name.git", "org/name"},
		{"git@github.com:org/name.git", "org/name"},
		{"https://github.com/org/name", "org/name"},
		{"https://github.com/org/name.git", "org/name"},
		{"", ""},
		// A NON-GITHUB host or a local path is returned UNCHANGED rather than
		// mangled. This normalises GitHub spellings; it does not assert that a
		// template must live on GitHub, and silently rewriting a self-hosted URL
		// into a bogus "org/name" would point copier at something that does not
		// exist.
		{"https://git.example.com/org/name", "https://git.example.com/org/name"},
		{"/local/path/to/template", "/local/path/to/template"},
	} {
		if got := NormalizeTemplateRepo(tc.in); got != tc.want {
			t.Errorf("NormalizeTemplateRepo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
