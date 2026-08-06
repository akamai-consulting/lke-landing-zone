package sustain

// deps.go — what the sustain verbs are handed.
//
// Smaller than teardown's, and the shape is the point: nothing here reaches a
// cluster or a cloud. Sustain answers "where did this instance come from, and how
// far behind is it" — repo questions, resolved from files and git.

// Answers is the slice of `.copier-answers.yml` this package reads.
//
// A NARROW COPY of package main's `answers`, deliberately. That type has 17 call
// sites across 10 files and is genuinely core — the instance provenance model many
// verbs read — so it does not move. Declaring the three fields actually used here
// keeps the dependency to what is consumed rather than to whatever the struct
// grows next, and the adapter in package main is four lines.
type Answers struct {
	Commit  string // _commit — the template SHA this instance rendered from
	SrcPath string // _src_path — the template repo
	Version string // llz_version — the release tag, when there is one
}

// Deps carries what this package cannot reach for itself.
type Deps struct {
	// ReadAnswers reads an instance's copier answers. Nil-safe by contract: a
	// directory with no `.copier-answers.yml` returns (nil, nil), because a
	// template-repo checkout legitimately has none and drift falls back to git.
	ReadAnswers func(dir string) (*Answers, error)

	// Exec captures a command's stdout — git, mostly.
	Exec func(name string, args ...string) ([]byte, error)

	// Run streams a command through to the terminal, for the verbs that show
	// their work (`copier update` and friends).
	Run func(argv []string, stdin string) error

	// Confirm is `--yes`. The same authorisation-not-capability seam teardown
	// found, and its second independent occurrence: `llz upgrade` rewrites files
	// in the operator's repo, so being ABLE to and being ASKED to are different
	// questions here too.
	Confirm func() bool
}
