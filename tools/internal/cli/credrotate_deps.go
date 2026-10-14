package cli

// credrotate_deps.go — wires internal/credrotate's one seam.
//
// Package main owns the forge credentials and the secret writer; credrotate owns
// WHICH secrets a rotation must reach — every infra-<deployment> environment, or
// the repo level on pre-env-scoped instances. That split is why the fan-out lives
// on the package side and the writer stays here.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
)

// NATIVE, NOT THE gh CLI. `ghsecret.SetFn` shells out to `gh`, and the caller
// that matters most for this seam is the broad-PAT rotator, which runs IN-CLUSTER
// from the `llz` image — `gcr.io/distroless/static-debian12:nonroot`, containing
// the llz binary and nothing else. There is no gh there and never will be, so the
// publish step could not succeed: exec returned "executable file not found" on
// every rotation, and the e2e broad-pat lane failed on it.
//
// ADR 0001 makes that publish load-bearing rather than incidental — the broad
// rotator mints each deployment's narrow PAT and publishes it to that
// deployment's environment secret — so the fix is the native writer, not moving
// the work. ghsecret.SetEnvNative goes over the REST API through
// forge.GitHubSecretWriter (sealed-box encryption, GHES-aware APIBase, and the
// LLZ_FORGE_STRICT guard against cross-lane clobber). It needs only GH_TOKEN and
// GH_REPO, which CI and the in-cluster pod both already have — so this is
// strictly wider than what it replaces, not a special case for one caller.
func init() {
	credrotate.Install(ghsecret.SetEnvNative)
}
