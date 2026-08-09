package pathglob

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{".github/workflows/*.yml", ".github/workflows/lint.yml", true},
		{".github/workflows/*.yml", ".github/workflows/sub/x.yml", false},
		{"template-scripts/**/*.sh", "template-scripts/lib.sh", true},
		{"template-scripts/**/*.sh", "template-scripts/ci/install.sh", true},
		{"template-scripts/**/*.sh", "template-scripts/a/b/c/x.sh", true},
		{"template-scripts/**/*.py", "template-scripts/x.go", false},
		{".github/actions/**/action.yml", ".github/actions/x/action.yml", true},
		{".github/actions/**/action.yml", ".github/actions/x/y/action.yml", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"x?.sh", "x1.sh", true},
		{"x?.sh", "x12.sh", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.path, func(t *testing.T) {
			if got := Match(tt.pattern, tt.path); got != tt.want {
				t.Errorf("Match(%q,%q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// TestMatchGlobStarAtPatternEdges covers the `*` lookahead at both ends of a
// pattern — a leading `*` (there is nothing before it to inspect) and a trailing
// `*` (there is nothing after it). Both are ordinary budget-config include
// patterns, and either edge mishandled makes matchGlob's error path swallow the
// pattern, silently dropping a whole category's files from the tally.
func TestMatchGlobStarAtPatternEdges(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"*.sh", "lib.sh", true},
		{"*.sh", "scripts/lib.sh", false}, // a single * stays within a segment
		{"scripts/*", "scripts/deploy.sh", true},
		{"scripts/*", "scripts/sub/deploy.sh", false},
		{"**", "a/b/c.sh", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.path, func(t *testing.T) {
			if got := Match(tt.pattern, tt.path); got != tt.want {
				t.Errorf("Match(%q,%q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// MatchAny is what every include/exclude list actually calls, and it was never
// exercised directly while it lived beside its own callers — the budget tests hit
// it only through a full scan, where a wrong answer looks like a wrong tally.
func TestMatchAny(t *testing.T) {
	patterns := []string{"tools/cmd/llz/*_test.go", "**/vendor/**"}
	tests := []struct {
		path string
		want bool
	}{
		{"tools/cmd/llz/a_test.go", true},
		{"a/vendor/b/c.go", true},
		{"tools/cmd/llz/a.go", false},
		{"vendored/x.go", false}, // `vendor` is a whole segment, not a prefix
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := MatchAny(patterns, tt.path); got != tt.want {
				t.Errorf("MatchAny(%v, %q) = %v, want %v", patterns, tt.path, got, tt.want)
			}
		})
	}
	if MatchAny(nil, "anything") {
		t.Error("an empty pattern list must match nothing — an absent `exclude:` cannot exclude everything")
	}
}

// Regexp metacharacters in a pattern are LITERAL path characters. This is the
// property that keeps the config files readable — an author writing
// `terraform-iac-bootstrap/*/.terraform.lock.hcl` is thinking about paths, not
// about escaping a dot — and it is why compile()'s error branch has no test: every
// character the translation does not interpret is escaped, so no pattern reaches
// it. If that ever stops being true, these cases are what will notice.
func TestMetacharactersAreLiteralPathCharacters(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"a[b", "a[b", true},
		{"a.go", "a.go", true},
		{"a.go", "axgo", false}, // a literal dot, not "any character"
		{"a+b", "a+b", true},
		{"(x)", "(x)", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.path, func(t *testing.T) {
			if got := Match(tt.pattern, tt.path); got != tt.want {
				t.Errorf("Match(%q,%q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}
