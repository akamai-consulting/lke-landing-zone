package forge

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The App JWT's clock claims are the contract with GitHub's token endpoint and
// nothing else asserts them: iat is backdated 60s so a runner clock that is a
// little fast does not present a future-dated iat (GitHub rejects the JWT
// outright), and exp is now+9m — deliberately inside GitHub's 10m ceiling, which
// it rejects at or above. TestGitHubAppMinter_InstallationToken only checks iss
// and the signature, so the two durations were free to drift.
func TestGitHubAppMinter_JWTClockClaims(t *testing.T) {
	pemBytes, _ := testAppKeyPEM(t)
	gh, _ := New(GitHub, "")
	m, err := NewGitHubAppMinter(gh, "12345", "67890", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	jwt, err := m.signAppJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts, want 3", len(parts))
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}

	if want := now.Unix() - 60; claims.Iat != want {
		t.Errorf("iat = %d, want %d (now backdated 60s for clock skew; an iat at or after now can be rejected as future-dated)",
			claims.Iat, want)
	}
	if want := now.Unix() + 9*60; claims.Exp != want {
		t.Errorf("exp = %d, want %d (now+9m)", claims.Exp, want)
	}
	// Belt and braces on the ceiling itself: GitHub rejects a JWT whose exp is
	// more than 10 minutes past its own clock, so exp must stay inside 10m of the
	// minter's now (the 60s backdate applies to iat, not to the ceiling).
	if ahead := claims.Exp - now.Unix(); ahead <= 0 || ahead > 600 {
		t.Errorf("exp is %ds past now, want 0 < exp-now <= 600 (GitHub's App JWT ceiling)", ahead)
	}
	if claims.Exp <= claims.Iat {
		t.Errorf("exp %d must be after iat %d", claims.Exp, claims.Iat)
	}
}

// The minter's HTTP client must carry a real timeout: it runs in-cluster on the
// distroless image (the rotator CronJob), where a hung GitHub connection with no
// client deadline blocks the job until the pod's deadline kills it.
func TestGitHubAppMinter_ClientTimeoutIsSet(t *testing.T) {
	pemBytes, _ := testAppKeyPEM(t)
	gh, _ := New(GitHub, "")
	m, err := NewGitHubAppMinter(gh, "1", "2", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	if to := m.client.Timeout; to <= 0 || to > time.Minute {
		t.Errorf("client timeout = %v, want a bounded non-zero timeout (0 means wait forever)", to)
	}
}
