package envdef

// extension.go — `env-definition` declares itself: the writer that turns `llz env
// add`'s answers into the YAML an instance is assembled from.
//
// SIXTY-NINTH EXTENSION, AND THE RISKIEST RENAME IN THE CAMPAIGN. `Opts` came with
// it — eighteen fields, read by five files — because it is the boundary between
// the FLAG SET (package main's, and it stayed) and the WRITER (this). Same call
// already made for internal/bootstrapcluster's BootstrapFlags.
//
// HOW THE RENAME WAS DONE, because the method mattered more than usual. A blanket
// `o.<field>` pass was not safe: `envAddOpts` has a `region` field and so do
// `openbaoLoginOpts` and `ReapOpts`, and that exact collision had already bitten
// twice. So the type moved first and the COMPILER drove the repair — each round
// applied only what `go build` reported, by exact file and line. Six rounds, and
// the two `ha*` fields needed a separate pass because `haRole` → `HARole` is too
// far apart for Go to offer a "but does have" hint.
//
// IT STILL WENT WRONG ONCE, in the way this campaign keeps rediscovering: a
// `<field>:` rewrite matched YAML KEYS INSIDE STRING LITERALS, so the emitted
// document said `Region:` where the spec schema says `region:`. `go build` was
// clean; one test caught it. The repair used the pre-move file as the authority
// and is verified by diffing every quoted literal against it — that diff is empty.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `env-definition` declaration.
//
//	transition:scaffolded[read-repo, write-repo]
//
// `write-repo` AT `scaffolded` — one of only two states that grant carries, and
// this is squarely one of them. The package creates `landingzone.yaml` when it is
// absent and writes `environments/<env>.yaml`: files in a working tree, which is
// exactly the distinction `write-repo` was added to draw against the GitHub API
// writes that take `cloud-mutate`.
//
// NO `cloud-read` DESPITE THE VALUES BEING CLOUD FACTS. The region, node type and
// object-storage cluster arrive already resolved — `instance-resolve` is the
// extension that asks Linode, and it is a separate declaration precisely so this
// one can be a pure file writer with no network at all.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "env-definition",
		Short:  "write landingzone.yaml and environments/<env>.yaml from the answers `llz env add` collected",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
		}},
	}
}
