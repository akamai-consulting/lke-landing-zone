package environments

// deps.go — the one edge this package could not bring with it.

// SyncPromoteWorkflow regenerates promote.yml after an environment is added.
//
// INJECTED, AND THE REASON IS ON RECORD. An earlier move tried to push this into
// internal/promote so it could be imported directly; that package's own
// TestPackageContainsNoWritePath refused it, because promote declares
// `transition:promoted[read-repo]` and the file write is deliberately outside it
// to keep that declaration TRUE. Changing the declaration is not available either:
// `write-repo` is legal at {scaffolded, upgraded}, not at `promoted`.
//
// So the write stays in package main and this package asks for it. The default is
// a NO-OP returning (false, nil) — "nothing changed" — because an env-add that
// silently skipped the pipeline regeneration is better than one that invents a
// result, and package main installs the real thing at init.
var SyncPromoteWorkflow = func(tfDir, relPrefix string, check bool) (bool, error) { return false, nil }
