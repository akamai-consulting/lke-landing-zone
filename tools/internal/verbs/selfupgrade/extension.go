package selfupgrade

// extension.go — `self-upgrade` declares itself: keep the llz binary current, and
// decide what an instance upgrade is allowed to overwrite.
//
// SEVENTY-FIRST EXTENSION. `selfupdate.go` + `upgrade_policy.go`, 573 lines,
// closure 5 → 4 once `run` moved to internal/proc.
//
// THAT ENABLER WAS SEVEN LINES with twenty-one call sites: print `→ <quoted argv>`
// to stderr, return early under --dry-run, otherwise exec. It was `run` in
// commands.go — a name so generic the closure scanner could not tell it from a
// method and neither could a reader. It is `proc.RunEcho` now, and the dry-run
// check travels WITH the announcement, which is the point: a caller that prints
// without checking, or checks without printing, is a bug you find by running the
// thing you were trying not to run.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `self-upgrade` declaration.
//
//	transition:upgraded[read-repo, write-repo, cloud-read]
//
// `write-repo` AT `upgraded` — the second of the only two states that grant
// carries, and the one it was added for. `overwriteManagedFromScaffold` copies
// template-owned files over the instance's working tree, which is precisely a
// working-tree write and precisely what `write-repo` means.
//
// `cloud-read` for the release lookup: `llzver.LatestRelease` asks GitHub what the newest
// llz tag is. That is why this is not a gate — a gate is cheap and offline.
//
// THE POLICY HALF IS THE INTERESTING HALF. `ApplyManifestPolicy` decides, per
// file, whether an upgrade may overwrite: managed files are template-owned and get
// replaced, instance-owned files are never touched, and the middle cases merge.
// It reads that classification from `template-manifest` rather than deciding it
// here — one table, one owner — which is why an unclassified file is a
// template-manifest failure and not a silent overwrite.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "self-upgrade",
		Short:  "update the llz binary, and apply the ownership policy that decides what an instance upgrade may overwrite",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Upgraded,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.WriteRepo, extension.CloudRead,
			},
		}},
	}
}
