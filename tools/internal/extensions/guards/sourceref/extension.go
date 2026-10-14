package sourceref

// extension.go — `guard-source-refs` declares itself.
//
// EARNED BY A MEASURED ROT, not by seeming plausible. The move of package main
// into tools/internal/** left ~70 path literals across docs, ADRs, chart
// comments, workflow comments, shell headers and skill files naming files that
// no longer existed — three of them naming files gone under EVERY path. Nothing
// caught it, and nothing could: docs-guard checks Markdown LINKS, flags,
// workflow dispatches and TOCs, and a path written as prose or inside a YAML
// comment is none of those.
//
// WHAT MAKES THE CLASS EXPENSIVE is not the broken pointer, it is the
// INSTRUCTION attached to it. The worst instance read "add new seeds THERE, not
// as steps here" beside a path that had not existed for the whole extraction — an
// instruction nobody can follow, in a file only opened while changing the thing
// it governs. A stale link is a dead end; a stale pointer with a direction on it
// sends the reader somewhere wrong.
//
// TWO COMMANDS, ONE BINDING, because they are the same claim about two halves of
// a reference. `source-ref-guard` resolves the PATH and `symbol-ref-guard` the
// `pkg.Symbol`, and a reference can be right about one and wrong about the other
// — the sweep that fixed the paths shipped exactly that, naming the file a symbol
// had left. They read the same corpus through the same gate binding and there is
// no instance where you would want one without the other.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-source-refs` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, and the kind fits without argument: the check opens files, extracts
// strings and asks the filesystem whether they resolve. No cluster, no cloud, no
// credential, findings out. `read-repo` and nothing else, which is what
// Validate() requires of a gate binding.
//
// `scaffolded` because a path literal is wrong the moment it is written, not at
// any later state. Every other guard here attaches at the same state for the
// same reason.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-source-refs",
		Short:  "a path or pkg.Symbol naming this repo's source must resolve to something that exists",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}

// sourceRefBinding is the gate binding this guard reads through, looked up rather
// than reconstructed so a second binding cannot silently widen it.
func sourceRefBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("guard-source-refs: no gate binding — the scan builds its read-repo " +
		"reader from it, so its absence is a wiring bug")
}
