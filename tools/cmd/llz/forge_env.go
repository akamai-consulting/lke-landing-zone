package main

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
)

// forgeFromEnv resolves the instance forge for CI-side llz commands, defaulting
// to GitHub.com. LLZ_FORGE selects the flavor and LLZ_FORGE_HOST the host
// (required for GHES/GitLab). Today every instance is github and forge.Supported
// gates the rest, so in practice this returns GitHub — it exists so the OIDC
// config (bao-configure) and OIDC-audience minting adopt a non-github forge the
// moment the spec is allowed to name one, rather than staying hardcoded.
// See docs/designs/forge-abstraction.md (Phase 3).
func forgeFromEnv() (forge.Forge, error) {
	flavor := forge.Flavor(os.Getenv("LLZ_FORGE"))
	if flavor == "" {
		flavor = forge.GitHub
	}
	return forge.New(flavor, os.Getenv("LLZ_FORGE_HOST"))
}
