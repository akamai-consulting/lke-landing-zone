package statepassphrase

import (
	"os"
	"strings"
)

// Deps carries what this package cannot reach for itself.
//
// TWO FIELDS OUT OF SIX CANDIDATES. The measured closure named six package main
// symbols; the three-clause rule (can the package already do this with a grant it
// holds? is it a pure function? is it already injectable elsewhere?) removed four:
//
//   - `appendGHAFile` is a file append to a path the ENVIRONMENT names. Localised
//     below — the fifth package in this campaign to reach that conclusion, and for
//     the reason internal/envtopology wrote down: a no-op default turns every test
//     asserting on the summary into a tautology, so the copy does the real thing.
//   - `ghHost` reads $GH_HOST with a default. Four lines, pure.
//   - `isNotFoundErr` is `strings.Contains(err.Error(), "HTTP 404")`. One line, pure.
//   - `globalOpts` was reached for two BOOLEANS, `dryRun` and `yes`. Flags are not
//     capabilities: they moved onto Opts, where the caller sets them, rather than
//     dragging package main's whole flag struct across the boundary.
type Deps struct {
	// Exec captures a command's stdout — `gh api`, for the environment-secret
	// writes and deletes this rollover performs.
	Exec func(name string, args ...string) ([]byte, error)

	// GHJSONPaged GETs a paginated GitHub API path and decodes the merged result.
	// Owned by package main's forge layer; this package asks the question and does
	// not know how to follow a Link header.
	GHJSONPaged func(path string, out any) error
}

// caps is the installed capability set. Defaults do something harmless and
// non-nil — an installed default is a fixture too.
var caps = Deps{
	Exec:        func(string, ...string) ([]byte, error) { return nil, nil },
	GHJSONPaged: func(string, any) error { return nil },
}

// Install wires the capabilities main owns. Call once, before any verb runs.
func Install(d Deps) { caps = d }

// ── localised pure helpers: copies, not seams ───────────────────────────────

// ghHost is the GitHub host llz authenticates against — github.com unless GH_HOST
// overrides it (e.g. a GHE-hosted template fork).
func ghHost() string {
	if h := strings.TrimSpace(os.Getenv("GH_HOST")); h != "" {
		return h
	}
	return "github.com"
}

// isNotFoundErr reports whether a `gh api` failure was a 404.
func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}
