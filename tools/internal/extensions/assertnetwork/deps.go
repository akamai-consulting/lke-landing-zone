package assertnetwork

// Deps carries what this package cannot reach for itself.
//
// TWO FIELDS, both shell-outs, and nothing else — which is what an assertion
// family looks like when the closure measurement (6 across 1,267 lines) is
// telling the truth. Everything else these four lanes need is stdlib or already
// behind internal/kubectlprobe's own seam.
//
// Installed rather than threaded: the lanes are leaf predicates several frames
// below their entry points, the same shape internal/converge documented.
type Deps struct {
	// Exec captures a command's stdout. The classified reads go through
	// internal/kubectlprobe; this is for calls wanting raw output.
	Exec func(name string, args ...string) ([]byte, error)

	// ExecCombined runs a command returning stdout+stderr as one string, ignoring
	// exit status.
	//
	// IT IS THE POINT OF THE net-probe LANE, not a diagnostic convenience. That
	// lane asserts a connection is REFUSED: it runs a probe pod and reads the
	// failure text to tell "connection refused" (the NetworkPolicy works) from
	// "no route to host" or a timeout (it may not). An error-gated, stdout-only
	// read discards exactly the evidence the assertion is made of.
	ExecCombined func(name string, args ...string) string
}

// deps is the installed capability set. Defaults do the real thing rather than
// returning zero values — an installed default is a fixture too, and defaulting a
// capability to "" has now broken a test in three separate extractions.
var deps = Deps{
	Exec:         func(string, ...string) ([]byte, error) { return nil, nil },
	ExecCombined: func(string, ...string) string { return "" },
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { deps = d }
