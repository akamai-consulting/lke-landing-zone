package openbao

// seed_deps.go — what the OpenBao seeders cannot reach for themselves.
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

	// KubectlCreate writes a manifest that must NOT replace an existing object,
	// returning kubectl's combined output so the caller can recognise AlreadyExists.
	// The static seal key is the reason it exists: apply is an upsert, so two seed
	// runs that both find the Secret absent both write, and the second destroys the
	// key that decrypts the first one's seal.
	KubectlCreate = func(manifest string) (string, error) {
		return "", fmt.Errorf("baoseed: KubectlCreate not installed")
	}

	// SetGitHubSecret writes a secret, scoped to an environment when env != "" —
	// where the seal key and the recovery material are escrowed.
	SetGitHubSecret = func(name, env, value string) error {
		return fmt.Errorf("baoseed: SetGitHubSecret not installed")
	}
)

// Install wires the capabilities main owns. Call once, before any seed runs.
func Install(apply func(string) error, create func(string) (string, error), setSecret func(name, env, value string) error) {
	KubectlApply, KubectlCreate, SetGitHubSecret = apply, create, setSecret
}

// ── localised pure helpers: copies, not seams ──────────────────────────────
