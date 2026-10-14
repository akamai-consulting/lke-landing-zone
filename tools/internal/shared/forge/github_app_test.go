package forge

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAppKeyPEM(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return pemBytes, key
}

func TestNewGitHubAppMinter_Requires(t *testing.T) {
	pemBytes, _ := testAppKeyPEM(t)
	gh, _ := New(GitHub, "")
	gl, _ := New(GitLab, "gitlab.corp")
	if _, err := NewGitHubAppMinter(gl, "1", "2", pemBytes); err == nil {
		t.Error("must reject a non-GitHub forge")
	}
	if _, err := NewGitHubAppMinter(gh, "", "2", pemBytes); err == nil {
		t.Error("must require app id")
	}
	if _, err := NewGitHubAppMinter(gh, "1", "2", []byte("not a pem")); err == nil {
		t.Error("must reject a bad private key")
	}
}

// The exchange must present a well-formed RS256 App JWT (verifiable with the
// public key, iss = app id) at the installation's token endpoint, and return the
// parsed token + expiry.
func TestGitHubAppMinter_InstallationToken(t *testing.T) {
	pemBytes, key := testAppKeyPEM(t)
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token": "ghs_installation", "expires_at": "2026-07-17T01:00:00Z",
		})
	}))
	defer srv.Close()

	gh, _ := New(GitHub, "")
	m, err := NewGitHubAppMinter(gh, "12345", "67890", pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	m.apiBase = srv.URL
	m.now = func() time.Time { return time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC) }

	tok, exp, err := m.InstallationToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_installation" {
		t.Errorf("token = %q", tok)
	}
	if !exp.Equal(time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("expiry = %v", exp)
	}
	if gotPath != "/app/installations/67890/access_tokens" {
		t.Errorf("path = %q", gotPath)
	}

	// Verify the JWT the minter presented.
	jwt := strings.TrimPrefix(gotAuth, "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	_ = json.Unmarshal(claimsJSON, &claims)
	if claims.Iss != "12345" {
		t.Errorf("iss = %q, want the app id 12345", claims.Iss)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("JWT signature does not verify: %v", err)
	}
}

func TestGitHubAppMinter_ImplementsTokenMinter(t *testing.T) {
	pemBytes, _ := testAppKeyPEM(t)
	gh, _ := New(GitHub, "")
	m, _ := NewGitHubAppMinter(gh, "1", "2", pemBytes)
	var _ TokenMinter = m // compile-time; also assert GitHub is not a rotator
	if _, ok := interface{}(m).(TokenRotator); ok {
		t.Error("GitHub App minter must not implement TokenRotator")
	}
}
