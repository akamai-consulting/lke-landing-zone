// Package pathglob matches slash-separated paths against the glob dialect this
// repo's config files are written in: `**` (any number of path segments,
// including zero), `*` (within one segment) and `?`, anchored at both ends.
//
// It exists as its own package because two unrelated gates depend on the SAME
// dialect and must not drift: the budget gates read include/exclude lists out of
// .untestable-budget.yaml and .core-surface-budget.yaml, and `llz ci
// template-manifest` matches copier's `_skip_if_exists` / `_exclude` entries with
// it. When both copies lived in package main that agreement was accidental.
//
// filepath.Match has no `**`, which is the whole reason these compile to a regexp
// instead.
package pathglob

import (
	"regexp"
	"strings"
)

func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}

// Match matches a slash-path against a glob supporting `**` (any number of
// path segments, including zero), `*` (within a segment), and `?`. Anchored at
// both ends. filepath.Match lacks `**`, so we compile to a regexp.
func Match(pattern, path string) bool {
	re, err := compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

var cache = map[string]*regexp.Regexp{}

func compile(pattern string) (*regexp.Regexp, error) {
	if re, ok := cache[pattern]; ok {
		return re, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// `**` — any sequence including slashes. Swallow a following
				// slash so `a/**/b` also matches `a/b`.
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*") // single * stays within a path segment
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	cache[pattern] = re
	return re, nil
}
