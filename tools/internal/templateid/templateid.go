// Package templateid holds this template's own identity: its repository name and
// the org that publishes it.
//
// Two consts with TWENTY-THREE references in package main, and they are FACTS —
// the same call baoread.Namespace, docsguard.DeliveredDocs and
// reconciler.FirewallConfigMapName got. Anything that needs to name the template
// has to agree, and the only way two callers can disagree is if there are two
// copies.
//
// They moved because `resolveInstanceRepo` could not: that function answers "which
// repo is this instance" from the copier answers, falling back to the template's
// own example repo, and the fallback was the one thing keeping it in package main.
package templateid

// Name is the template repository's name, without an org.
const Name = "lke-landing-zone"

// DefaultOrg publishes the template. It is the fallback when an instance has no
// recorded repo — an admin-mode convenience, never a value written into an
// instance, which is why nothing here is configurable: an instance that needs a
// different org records it in .copier-answers.yml.
const DefaultOrg = "akamai-consulting"

// ExampleRepo is the owner/name the admin fallback resolves to.
func ExampleRepo() string { return DefaultOrg + "/" + Name + "-example" }
