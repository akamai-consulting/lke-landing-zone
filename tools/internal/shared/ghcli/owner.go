package ghcli

// owner.go — two small `gh` helpers that were in commands.go.
//
// NotFound classifies a `gh` exit error as a 404, and OwnerKind asks GitHub
// whether an owner is a User or an Organization. Seven lines and ten, two callers
// each, and both about the gh CLI — which is what this package is for.
//
// NotFound IS THE INTERESTING ONE. It reads exec.ExitError.Stderr and matches
// "HTTP 404" or "Not Found", which is a string match against another program's
// output — fragile by nature. Having it in ONE place is the only thing that makes
// it maintainable: when gh changes its wording, there is a single line to fix
// rather than a search for everyone who guessed at the same match.

import (
	"errors"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ghOwnerKind classifies a GitHub account name: "User", "Organization", or ""
// when GitHub definitively 404s it. A non-nil error means indeterminate (no `gh`
// auth, offline, rate-limited) — callers must not treat that as "absent".
// /users/<name> resolves orgs too, so one call answers both questions.
// OwnerKindFn is the seam. It lives HERE, beside the function it wraps, rather
// than in each caller — two swappable vars over one call is how a test stubs one
// and leaves the other reaching GitHub for real.
var OwnerKindFn = OwnerKind

func OwnerKind(owner string) (string, error) {
	out, err := kubectlprobe.Exec("gh", "api", "users/"+owner, "--jq", ".type")
	if err != nil {
		if NotFound(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ghNotFound reports whether a `gh api` failure was a 404 rather than an auth,
// network, or rate-limit problem — gh writes "gh: Not Found (HTTP 404)" to
// stderr, which (*exec.Cmd).Output captures on the ExitError.
func NotFound(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		s := string(ee.Stderr)
		return strings.Contains(s, "HTTP 404") || strings.Contains(s, "Not Found")
	}
	return false
}
