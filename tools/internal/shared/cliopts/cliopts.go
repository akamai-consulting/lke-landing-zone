// Package cliopts holds the three process-wide flags every command may read:
// --dry-run, --yes and --open.
//
// IT EXISTS BECAUSE THE READ MUST BE LATE. Package main binds these to the root
// command's PERSISTENT flags, which cobra parses when a command EXECUTES — long
// after every constructor has run. A command that took `dryRun bool` as a
// constructor argument would therefore capture the pre-parse zero value, and
// `--dry-run` would silently do nothing. Nothing would fail; the flag would just
// stop working, on a cloud-mutating command, which is the worst place for a
// silent default.
//
// So this is a VALUE read at RunE time, not a parameter threaded at construction
// time. That is the opposite of how the rest of the extraction moved things —
// `newinstance.Run(dryRun, yes bool, …)` takes them as arguments — and the
// difference is exactly the binding moment: those are called from inside a RunE
// that has already read the globals, and these ARE the globals.
//
// It is a struct rather than three bare vars so `globalOpts` in package main can
// stay a type alias for it and the ~50 functions taking one keep their signature.
package cliopts

// Opts is the global flag set.
type Opts struct {
	DryRun bool
	Open   bool
	Yes    bool
}

// Global is what package main binds the root command's persistent flags to, and
// what every extension's command reads inside its RunE.
//
// A package-level var, not an injected seam, and deliberately: an injected copy
// would be assigned at wiring time and freeze the same pre-parse zeros this
// package exists to avoid. Tests set it directly and restore it with t.Cleanup.
var Global Opts
