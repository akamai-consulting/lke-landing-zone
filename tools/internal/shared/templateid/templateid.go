// Package templateid holds this template's own identity: its repository name and
// the org that publishes it.
//
// Two consts with TWENTY-THREE references in package main, and they are FACTS —
// the same call baoread.Namespace, platform.DeliveredDocs and
// platform.FirewallConfigMapName got. Anything that needs to name the template
// has to agree, and the only way two callers can disagree is if there are two
// copies.
//
// They moved because `resolveInstanceRepo` could not: that function answers "which
// repo is this instance" from the copier answers, falling back to the template's
// own example repo, and the fallback was the one thing keeping it in package main.
package templateid

import "strings"

// Name is the template repository's name, without an org.
const Name = "lke-landing-zone"

// DefaultOrg publishes the template. It is the fallback when an instance has no
// recorded repo — an admin-mode convenience, never a value written into an
// instance, which is why nothing here is configurable: an instance that needs a
// different org records it in .copier-answers.yml.
const DefaultOrg = "akamai-consulting"

// Repo is the template's own owner/name. Six files in package main built this
// string, and one of them built ExampleRepo from the same two halves — the shape
// that lets two callers disagree about one fact.
func Repo() string { return DefaultOrg + "/" + Name }

// ExampleRepo is the owner/name the admin fallback resolves to.
func ExampleRepo() string { return DefaultOrg + "/" + Name + "-example" }

// DefaultRepo is "<DefaultOrg>/<Name>", DERIVED rather than written out.
//
// IT WAS WRITTEN OUT, in internal/extensions/sustain, as
// `const DefaultTemplateRepo = "akamai-consulting/lke-landing-zone"` -- a third
// copy of the two facts directly above, and precisely the thing this package's
// own header warns about: "Anything that needs to name the template has to agree,
// and the only way two callers can disagree is if there are two copies." There
// were. Deriving it means the org and the name have exactly one definition each
// and this cannot drift from them.
const DefaultRepo = DefaultOrg + "/" + Name

// NormalizeTemplateRepo reduces the many ways a template repo can be spelled --
// `gh:org/name`, `git@github.com:org/name.git`, an https URL, a bare `org/name` --
// to the bare form the two consts above are in.
//
// It came from sustain with the const it belongs to. A non-github host or a local
// path is returned UNCHANGED rather than mangled: this normalises GitHub spellings,
// it does not assert that a template must live on GitHub.
// NormalizeTemplateRepo turns a copier _src_path / git remote into an owner/repo
// slug when it is a github reference; otherwise returns it unchanged (a non-github
// host stays a full URL, which `llz drift` handles).
func NormalizeTemplateRepo(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(s, "gh:"):
		return strings.TrimSuffix(strings.TrimPrefix(s, "gh:"), ".git")
	case strings.HasPrefix(s, "git@github.com:"):
		return strings.TrimSuffix(strings.TrimPrefix(s, "git@github.com:"), ".git")
	case strings.Contains(s, "github.com/"):
		return strings.TrimSuffix(s[strings.Index(s, "github.com/")+len("github.com/"):], ".git")
	default:
		return s // some other host / local path — leave as-is
	}
}
