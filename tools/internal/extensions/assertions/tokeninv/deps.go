package tokeninv

import "os"

// Deps carries what this package cannot reach for itself.
//
// Small, for a package this size. That is the shape of a set of checks that
// mostly READ credentials out of the environment and probe them over HTTP: the
// network probes are already package vars (tokenprobe.GHPATProbe, s3Probe) because they were
// written to be exercisable without a network, so the only real seams left are
// the Linode client and the summary sink.
type Deps struct {
	// CloudToken reads the Linode PAT the CI sweeps run under. A function rather
	// than a string because it can fail, and "no token configured" is a verdict
	// this extension reports rather than an error it dies on.
	CloudToken func() (string, error)

	// Summary appends to a GitHub Actions file (GITHUB_STEP_SUMMARY). It must
	// actually write: the rotation plan's whole output is the summary plus the
	// job-gating outputs, so a no-op fixture would leave every assertion about
	// either running against nothing.
	Summary func(envVar string, lines ...string) error
}

// envOr reads an env var with a default. package main has the same one-liner over
// an injected getenv; duplicating three lines is cheaper here than exporting a
// helper whose only purpose is to read os.Getenv, and there is no behaviour to
// drift.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// firstNonEmpty returns the first non-empty string. Same reasoning as envOr — the
// package-main original lives in tokens.go, which is the credential PROVISIONING
// wizard and deliberately stayed behind.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
