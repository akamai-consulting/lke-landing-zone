package cli

// commands_claimed_test.go — the ratchet on commands no extension declares.
//
// ────────────────────────────────────────────────────────────────────────────
// THIS CHECK WAS IMPOSSIBLE UNTIL THE TREE LEFT package main.
//
// command_wiring_test.go asserts one direction — every verb an extension exposes
// is reachable in the tree — and its own comment records why the reverse was left
// alone: "Asserting `every leaf is claimed` would mean maintaining a list of those
// by hand — the 214-entry exercise this design replaced — to catch a failure that
// has never happened."
//
// That was the right call THEN, and the reason was the location: in package main
// the list could only be hand-typed, and a hand-typed list of 81 entries is a
// second copy of the tree that drifts. The tree is an ordinary package now, so the
// list is GENERATED from it once and then ratcheted, which is a different object
// with a different failure mode: it cannot drift, because the test fails the moment
// it disagrees with the tree in EITHER direction.
//
// WHAT IT IS ACTUALLY FOR is measuring the extraction backlog. An unclaimed command
// is one that exists in the binary and in no declaration — invisible to `llz
// extension list`, to enablement, to the capability fence, and to `llz ci gates`.
// The count is the honest size of "how much of this CLI is outside the model", and
// a ratchet turns it into a countdown rather than an unknown: extracting a command
// FAILS this test until its entry is deleted, so the paydown gets banked instead of
// rotting.
//
// IT ALREADY EARNED ITS KEEP. Generating the first list surfaced `llz secrets
// onboard.Gather` — a command whose name had been corrupted by a symbol rename that
// hit string literals, while docs/quickstart.md and two Go comments went on calling
// it `llz secrets gather`. Nothing else in the tree noticed: it is a legal cobra
// name, so it built, shipped, and was simply unreachable as documented.
// ────────────────────────────────────────────────────────────────────────────

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

// unclaimedCommands is every command path in the tree that no extension declares.
//
// GENERATED, THEN FROZEN. It is the state of the tree at the move out of package
// main, and the only legal edits are DELETIONS as commands move into extensions.
// Adding a line is legal too — a new command that is genuinely core wiring — but it
// is a line in a diff a reviewer sees, which is the point.
//
// Paths, not bare names, because names are not unique: `create`, `get`, `exec` and
// `validate` each appear under more than one parent, and a bare-name list would let
// one of them be extracted while silently excusing its namesake.
var unclaimedCommands = []string{
	"apl",
	"apl app",
	"apl app disable",
	"apl app enable",
	"apl doctor",
	"apl openbao exec",
	"apl openbao get",
	"apl status",
	"apl values",
	"build",
	"check",
	"check actions-lint",
	"check checkov",
	"check conflict-markers",
	"check fmt-check",
	"check gitleaks",
	"check tf-lint",
	"check tf-validate",
	"check vendored-fresh",
	"ci",
	"ci assert-destroy-confirm",
	"ci assert-no-orphans",
	"ci bao-status",
	"ci build-failure-summary",
	"ci clear-cluster-secrets",
	"ci collect-image-pulls",
	"ci collect-timing",
	"ci destroy-unwedge",
	"ci diagnose-argocd",
	"ci diagnose-reconciler",
	"ci gates",
	"ci gen-toc",
	"ci managed-fresh",
	"ci mutate",
	"ci phase-mark",
	"ci phase-report",
	"ci require-secret",
	"ci teardown-capture",
	"ci teardown-delete-vpc",
	"ci teardown-force-delete",
	"ci temp-objkey create",
	"ci temp-objkey delete",
	"ci upgrade-test",
	"components",
	"credentials lke-admin",
	"credentials lke-admin rotate",
	"credentials obj-key",
	"credentials obj-key create",
	"credentials obj-key revoke-old",
	"credentials pat",
	"credentials pat create",
	"credentials pat revoke-old",
	"drift",
	"doctor",
	"env",
	"env next",
	"env pipeline",
	"env show",
	"extension",
	"fmt",
	"hooks",
	"import",
	"import init",
	"import plan",
	"import scan",
	"lint",
	"new",
	"openbao exec",
	"openbao get",
	"precommit",
	"secrets",
	"secrets gather",
	"secrets push",
	"self-update",
	"spec validate",
	"status",
	"tokens",
	"up",
	"upgrade",
	"validate",
	"version",
}

func TestUnclaimedCommandsMatchTheTree(t *testing.T) {
	claimed := map[string]bool{}
	for _, c := range registry.Commands() {
		claimed[c.New().Name()] = true
	}

	live := map[string]bool{}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		for _, k := range c.Commands() {
			p := strings.TrimSpace(path + " " + k.Name())
			if !claimed[k.Name()] {
				live[p] = true
			}
			walk(k, p)
		}
	}
	walk(newRootCmd(), "")

	allowed := map[string]bool{}
	for _, p := range unclaimedCommands {
		allowed[p] = true
	}

	var appeared, banked []string
	for p := range live {
		if !allowed[p] {
			appeared = append(appeared, p)
		}
	}
	for p := range allowed {
		if !live[p] {
			banked = append(banked, p)
		}
	}
	sort.Strings(appeared)
	sort.Strings(banked)

	if len(appeared) > 0 {
		t.Errorf("%d command(s) in the tree are declared by no extension and not listed:\n\t%s\n"+
			"\tA command outside the model is invisible to `llz extension list`, to enablement, "+
			"to the capability fence and to the gate driver. Declare it in an extension, or add "+
			"it to unclaimedCommands if it is genuinely core wiring.",
			len(appeared), strings.Join(appeared, "\n\t"))
	}
	if len(banked) > 0 {
		t.Errorf("%d listed command(s) are no longer unclaimed — DELETE them from "+
			"unclaimedCommands in this commit:\n\t%s\n"+
			"\tThat is the paydown this ratchet exists to bank. Slack left behind silently "+
			"pre-approves the next command that skips the model.",
			len(banked), strings.Join(banked, "\n\t"))
	}

	t.Logf("commands outside the extension model: %d", len(live))
}
