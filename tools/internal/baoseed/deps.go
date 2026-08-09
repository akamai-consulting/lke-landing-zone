package baoseed

// deps.go — what the OpenBao seeders cannot reach for themselves.
//
// TWO SEAMS OUT OF ELEVEN MEASURED SYMBOLS. The three-clause rule removed the
// rest: `appendGHAFile`, `maskGHA` and `maskGHALines` are printers (localised
// below), `openbaoNS` is now `baoread.Namespace`, `baoKVPutFn` is now
// `baoread.KVPut`, and `globalOpts` was reached for two booleans, which are FLAGS
// rather than capabilities and moved onto the call signatures.
//
// A third was written and then deleted: `SecretManifest`, for the generic Opaque
// Secret shape. The package renders its own seal-key manifest and never needed it
// — a seam added on the strength of the measurement rather than a call site, which
// is the cheapest kind of wrong to catch and the easiest to leave in.

import (
	"errors"
	"fmt"
	"os"
)

// errNotInstalled is what an un-wired capability returns. What matters is only
// that it is non-nil: a seeder that reports success without writing is the exact
// failure baoread's fail-closed discipline exists to prevent, one layer up.
var errNotInstalled = errors.New("baoseed: capability not installed")

var (
	// KubectlApply applies a manifest to the cluster. The seal-key and CA paths
	// write Kubernetes Secrets directly, before OpenBao is up enough to hold them.
	//
	// The default ERRORS rather than doing nothing. A seeder that reports success
	// without applying is the failure baoread's whole fail-closed discipline exists
	// to prevent, and an inert default reintroduces it one layer up.
	KubectlApply = func(manifest string) error { return fmt.Errorf("baoseed: KubectlApply not installed") }

	// SetGitHubSecret writes a secret, scoped to an environment when env != "" —
	// where the seal key and the recovery material are escrowed.
	SetGitHubSecret = func(name, env, value string) error {
		return fmt.Errorf("baoseed: SetGitHubSecret not installed")
	}
)

// Install wires the capabilities main owns. Call once, before any seed runs.
func Install(apply func(string) error, setSecret func(name, env, value string) error) {
	KubectlApply, SetGitHubSecret = apply, setSecret
}

// ── localised pure helpers: copies, not seams ──────────────────────────────

// appendGHAFile appends lines to the GitHub Actions command file named by envVar.
// THE REAL THING, not a stub — the seed summary is asserted on.
func appendGHAFile(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open $%s: %w", envVar, err)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			f.Close()
			return fmt.Errorf("write $%s: %w", envVar, err)
		}
	}
	return f.Close()
}

// maskGHA asks GitHub Actions to redact a value from the log.
func maskGHA(v string) {
	if os.Getenv("GITHUB_ACTIONS") != "" && v != "" {
		fmt.Printf("::add-mask::%s\n", v)
	}
}
