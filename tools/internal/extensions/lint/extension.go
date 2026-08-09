package lint

// extension.go — `lint` declares itself, and it is the first extension whose
// existence was hiding in plain sight.
//
// `llz lint` / `llz fmt` / `llz validate` / `llz check` shipped for the whole
// campaign as `checks.go` in package main, and NOTHING DECLARED THEM. They are
// not composition in the sense `up` and `import` are — those sequence other
// commands. This owns thirteen steps outright: the argv builders, the tool
// discovery, the conflict-marker scanner, and the ordering.
//
// IT IMPORTS ITS STEPS RATHER THAN RE-EXEC'ING THEM, which is the opposite of
// what `assert-suite` does, and the difference is worth stating because the two
// look alike. assert-suite re-execs `llz ci <lane>` as a subprocess so that
// concurrent lanes cannot interleave stdout, one lane's os.Exit cannot take the
// battery down, and each lane's package-level seams stay independent. None of
// those apply here: lint is SEQUENTIAL, fails fast, and its Go-level steps
// (pin-coherence, render-fresh, vendored-fresh, dropped-apiversions) are pure
// functions over the working tree. Paying subprocess cost for four calls that
// cannot interfere would buy nothing.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `lint` declaration.
//
//	gate:scaffolded[read-repo]                  llz lint / llz check / llz validate
//	transition:scaffolded[read-repo, write-repo] llz fmt
//
// TWO BINDINGS BECAUSE ONE VERB WRITES. `llz fmt` runs the formatters in fix
// mode — gofmt -w, tofu fmt — and rewrites files in place. Folding it into the
// gate would have put a write behind a `read-repo` declaration, which is the
// failure internal/extensions/docsguard's own guard caught when `gen-toc` tried
// to move in. `write-repo` is legal at `scaffolded`, so this needs no widening;
// it needs the second row.
//
// `llz hooks` and `llz precommit` arrived later and needed NEITHER a new row nor
// a new extension. precommit is RunLint plus a secret-path refusal, and hooks
// installs the shim whose only job is to call it — the gate covers one and the
// write row covers the other. That the write lands in .git/hooks rather than in
// tracked content is a detail the grant does not distinguish, and should not:
// arming a hook changes what a commit does, which is what write-repo is about.
//
// `gate`, not `assertion`, even though the steps shell out to gofmt/tflint/
// actionlint/gitleaks. The campaign's rule is that a gate is cheap and OFFLINE:
// these are local binaries on PATH, there is no network and no cluster, and this
// IS the pre-commit path the rule was written to describe. Every step is a no-op
// pass when its tool is absent, so an operator without tflint installed still
// gets a green gate rather than a false failure.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "lint",
		Short:  "the local gate: formatters, linters, secret scan, the tree's own guards, and the hook that arms them",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Gate,
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Transition,
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
			},
		},
	}
}
