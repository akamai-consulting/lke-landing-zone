package onboard

// Opts is the slice of package main's globalOpts this package actually reads.
//
// THREE FIELDS, NOT THE WHOLE STRUCT. Every other extraction in this tree took
// a bool or two; this one takes a named type because it reads three and threading
// three bare bools through a dozen frames is how a caller eventually swaps two of
// them. `Open` is the unusual one: it launches a browser at a token-creation page,
// which is exactly the kind of side effect a --dry-run must be able to suppress.
type Opts struct {
	DryRun bool
	Open   bool
	Yes    bool
}
