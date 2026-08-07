package forge

import (
	"testing"
)

// TestOIDCAudienceForRepoOwnerSplit pins how the owner is carved out of the
// GITHUB_REPOSITORY slug. The audience minted here must equal the
// bound_audiences `llz ci bao-configure` pinned on the OpenBao jwt role, so any
// silent widening (an empty owner collapses the audience to the bare
// https://github.com/, which is a DIFFERENT and broader value) breaks the login
// at exchange time with an opaque "no client_token".
func TestOIDCAudienceForRepoOwnerSplit(t *testing.T) {
	t.Setenv("LLZ_FORGE", "")
	t.Setenv("LLZ_FORGE_HOST", "")

	if got, want := OIDCAudienceForRepo("acme/platform"), "https://github.com/acme"; got != want {
		t.Errorf("OIDCAudienceForRepo(acme/platform) = %q, want %q", got, want)
	}
	// A slug with no owner segment at all is NOT an owner-less repo — it is a
	// malformed value, and must be carried through verbatim rather than
	// silently minting the bare org-wide audience.
	if got, want := OIDCAudienceForRepo("/platform"), "https://github.com//platform"; got != want {
		t.Errorf("OIDCAudienceForRepo(/platform) = %q, want %q — a leading '/' must not be read as an empty owner", got, want)
	}
	// No separator at all: the whole slug is the owner.
	if got, want := OIDCAudienceForRepo("acme"), "https://github.com/acme"; got != want {
		t.Errorf("OIDCAudienceForRepo(acme) = %q, want %q", got, want)
	}
}

// TestOIDCAudienceForRepoHonoursTheForge is the forge-abstraction half: GHES
// issues Actions OIDC tokens with the APPLIANCE as the audience, not
// https://github.com/<owner>. Minting the github.com shape against a GHES role
// produces a token whose aud never matches bound_audiences. The
// https://github.com/<owner> string is also the CONSERVATIVE FALLBACK used when
// the forge cannot be resolved, so the two must not be confusable.
func TestOIDCAudienceForRepoHonoursTheForge(t *testing.T) {
	t.Setenv("LLZ_FORGE", "github-enterprise-server")
	t.Setenv("LLZ_FORGE_HOST", "ghe.example.com")

	got := OIDCAudienceForRepo("acme/platform")
	if want := "https://ghe.example.com"; got != want {
		t.Errorf("GHES audience = %q, want %q — the resolved forge must win over the github.com fallback", got, want)
	}

	// An unresolvable forge falls back to the github.com shape rather than
	// panicking on a nil Forge.
	t.Setenv("LLZ_FORGE", "github-enterprise-server")
	t.Setenv("LLZ_FORGE_HOST", "") // GHES requires a host
	if got, want := OIDCAudienceForRepo("acme/platform"), "https://github.com/acme"; got != want {
		t.Errorf("unresolvable forge: audience = %q, want the conservative fallback %q", got, want)
	}
}
