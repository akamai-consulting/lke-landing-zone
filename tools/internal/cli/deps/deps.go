// Package deps assembles the capability Deps that command constructors are handed.
//
// ─────────────────────────────────────────────────────────────────────────────
// IT IS A SEPARATE PACKAGE SO THE REGISTRY CAN REACH IT, and that is the whole
// reason it exists rather than being more of internal/cli.
//
// `template-sustain` was one of three entries in registry/gates.go's undrivenGates
// for a reason that had nothing to do with the gate: "llz ci managed-fresh is
// assembled from main's sustainDeps(), which internal/shared cannot reach". The
// gate was perfectly driveable; the ASSEMBLY was in the one package nothing may
// import.
//
// Moving the assembly to internal/cli would not have fixed it. internal/cli
// imports the registry (for `llz ci gates` and `llz extension list`), so a registry
// that imported internal/cli back would be a cycle. A sibling package under cli/ is
// not: Go's import graph is per-PACKAGE, not per-directory, so
//
//	cli → registry → cli/deps        and        cli → cli/deps
//
// is acyclic, and the gate driver can hand itself the same Deps the CLI does.
//
// WHAT BELONGS HERE is assembly only — wiring a capability's Deps out of shared
// packages. Decision logic in here would be logic no extension declares and no
// binding grants, which is precisely what the model exists to stop.
// ─────────────────────────────────────────────────────────────────────────────
package deps

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/manifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
)

// Sustain is what internal/sustain is handed: provenance, two shell-outs, and the
// --yes bit. No cluster, no cloud — sustain answers repo questions.
func Sustain() sustain.Deps {
	return sustain.Deps{
		LockableScaffoldFiles: lockableScaffoldFiles,
		ReadAnswers: func(dir string) (*sustain.Answers, error) {
			a, err := answers.Read(dir)
			if err != nil || a == nil {
				return nil, err
			}
			return &sustain.Answers{Commit: a.Commit, SrcPath: a.SrcPath, Version: a.Version}, nil
		},
		Exec: kubectlprobe.Exec,
		Run:  proc.Run,
		// READ LATE, NOT CAPTURED. cliopts.Global is populated by flag parsing,
		// which happens after every constructor has run — a value read here at
		// assembly time would always be the zero one. See internal/shared/cliopts.
		Confirm: func() bool { return cliopts.Global.Yes },
	}
}

// lockableScaffoldFiles answers sustain's LockableScaffoldFiles: the scaffold root
// plus every file under it whose .template-manifest class is digest-locked.
//
// The class table is ADR 0014's single ownership authority and stays in
// shared/manifest; what crosses the boundary is the ANSWER, not the model.
func lockableScaffoldFiles(root string) (string, []string, error) {
	m, err := manifest.Load(root)
	if err != nil {
		return "", nil, err
	}
	files, err := manifest.ScaffoldFiles(m.Root)
	if err != nil {
		return "", nil, err
	}
	var out []string
	for _, rel := range files {
		if c, ok := manifest.LookupClass(m.Classify(rel)); ok && c.DigestLocked {
			out = append(out, rel)
		}
	}
	return m.Root, out, nil
}
