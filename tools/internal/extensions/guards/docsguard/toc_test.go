package docsguard

import (
	"strings"
	"testing"
)

func TestApplyTOC(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		level      int
		want       []string
		absent     []string
		changed    bool
	}{
		{
			name:    "inserts before the first h2, after the intro",
			body:    "# Title\n\nIntro prose.\n\n## Alpha\n\ntext\n\n## Beta\n",
			level:   2,
			want:    []string{"<!-- toc -->", "- [Alpha](#alpha)", "- [Beta](#beta)", "<!-- /toc -->"},
			changed: true,
		},
		{
			// The rule that broke when the generator was a separate script:
			// punctuation between two spaces leaves TWO hyphens.
			name:    "double hyphen survives round-trip",
			body:    "# T\n\n## Writing / rotating secrets — dual-write\n",
			level:   2,
			want:    []string{"(#writing--rotating-secrets--dual-write)"},
			changed: true,
		},
		{
			name:    "nested levels indent",
			body:    "# T\n\n## Alpha\n\n### Nested\n",
			level:   3,
			want:    []string{"- [Alpha](#alpha)", "  - [Nested](#nested)"},
			changed: true,
		},
		{
			name:    "level 2 omits deeper headings",
			body:    "# T\n\n## Alpha\n\n### Nested\n",
			level:   2,
			want:    []string{"- [Alpha](#alpha)"},
			absent:  []string{"#nested"},
			changed: true,
		},
		{
			name:    "the TOC never lists itself or See also",
			body:    "# T\n\n## Alpha\n\n## See also\n\n- x\n",
			level:   2,
			want:    []string{"- [Alpha](#alpha)"},
			absent:  []string{"#see-also", "#contents"},
			changed: true,
		},
		{
			name:    "duplicate headings get GitHub's -1 suffix",
			body:    "# T\n\n## Notes\n\n## Notes\n",
			level:   2,
			want:    []string{"(#notes)", "(#notes-1)"},
			changed: true,
		},
		{
			// A `## Foo` inside an example block is not a heading.
			name:    "fenced pseudo-headings are not listed",
			body:    "# T\n\n## Real\n\n```\n## Fake\n```\n",
			level:   2,
			want:    []string{"- [Real](#real)"},
			absent:  []string{"#fake"},
			changed: true,
		},
		{
			name:    "a link in a heading is unwrapped, since links cannot nest",
			body:    "# T\n\n## See [the guide](g.md)\n",
			level:   2,
			want:    []string{"- [See the guide](#see-the-guide)"},
			changed: true,
		},
		{
			name:    "a doc with no h2 gets no TOC",
			body:    "# T\n\nJust prose.\n",
			level:   2,
			absent:  []string{"<!-- toc -->"},
			changed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := applyTOC(tc.body, tc.level)
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in:\n%s", a, got)
				}
			}
		})
	}
}

// Regenerating must be a no-op, or `--check` would report every file stale on
// every run and the gate would be noise.
func TestApplyTOC_IsIdempotent(t *testing.T) {
	body := "# T\n\nIntro.\n\n## Alpha\n\n### Deep\n\n## Beta — with / punctuation\n"
	once, _ := applyTOC(body, 3)
	twice, changed := applyTOC(once, 3)
	if changed {
		t.Error("second pass reported a change; the block is not stable")
	}
	if once != twice {
		t.Errorf("not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// The generator and the checker must agree by construction — they share
// docHeadings. This asserts it end-to-end rather than trusting the refactor.
func TestGeneratedTOCSatisfiesTheGuard(t *testing.T) {
	body := "# T\n\n## Alpha / beta — gamma\n\n## `workflow_call` interface\n\n## Notes\n\n## Notes\n"
	out, _ := applyTOC(body, 2)
	var n Scanned
	if f := checkDocTOCs([]docFile{{rel: "d.md", body: out}}, &n); len(f) != 0 {
		t.Fatalf("docs-guard rejected a freshly generated TOC: %v", f)
	}
	if n.TOCEntries != 4 {
		t.Errorf("checked %d entries, want 4", n.TOCEntries)
	}
}
