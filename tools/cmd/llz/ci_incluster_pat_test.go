package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// oidcServer points the GitHub Actions OIDC request env at a fake endpoint that
// returns a fixed token, asserting the audience + bearer are sent.
func oidcServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("audience") == "" {
			http.Error(w, "missing audience", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer req-tok" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"value":"oidc-jwt"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "req-tok")
}

// patMintStub wraps stubLinode to record the mint arguments (label/scopes) and
// serve as BOTH client seams: newPATRotatorClient (mint + drain) and
// linodeRotatorClient (the verify probe on the freshly-minted token).
type patMintStub struct {
	stubLinode
	label, scopes, expiry string
}

func (s *patMintStub) CreateProfileToken(ctx context.Context, label, scopes, expiry string) (map[string]any, error) {
	s.label, s.scopes, s.expiry = label, scopes, expiry
	s.patCreates++
	return map[string]any{"id": jn(100 + s.patCreates), "token": "new-narrow-pat"}, nil
}

func withInclusterPATStubs(t *testing.T, s *patMintStub, now time.Time) {
	t.Helper()
	op, ol, on := newPATRotatorClient, linodeRotatorClient, rotatorNow
	newPATRotatorClient = func(string) patAPI { return s }
	linodeRotatorClient = func(string) rotatorLinodeAPI { return s }
	rotatorNow = func() time.Time { return now }
	t.Cleanup(func() { newPATRotatorClient, linodeRotatorClient, rotatorNow = op, ol, on })
}

// stubInclusterBaoExec fakes the in-pod bao CLI: `kv get` answers the
// skip-if-present probe with seededToken, `write` answers the jwt login, and
// every call is recorded.
func stubInclusterBaoExec(t *testing.T, seededToken, loginToken string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := baoExecFn
	baoExecFn = func(pod, token, stdin string, args ...string) (string, string, error) {
		calls = append(calls, append([]string{"pod=" + pod, "token=" + token, "stdin=" + stdin}, args...))
		switch args[0] {
		case "kv":
			if args[1] == "get" {
				if seededToken == "" {
					return "", "No value found", errors.New("exit 2")
				}
				return seededToken, "", nil
			}
			return "", "", nil
		case "write": // the auth/jwt/login call
			if loginToken == "" {
				return "", "permission denied", errors.New("exit 2")
			}
			return fmt.Sprintf(`{"auth":{"client_token":%q}}`, loginToken), "", nil
		}
		return "", "", errors.New("unexpected bao exec: " + strings.Join(args, " "))
	}
	t.Cleanup(func() { baoExecFn = prev })
	return &calls
}

func inclusterPATEnv(t *testing.T, broadToken string) string {
	t.Helper()
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")
	t.Setenv("LINODE_API_TOKEN", broadToken)
	t.Setenv("OPENBAO_ROOT_TOKEN", "root-tok")
	return sum
}

func TestMintBootstrapPATHappyPath(t *testing.T) {
	inclusterPATEnv(t, "broad-pat")
	s := &patMintStub{}
	now := time.Unix(1_800_000_000, 0)
	withInclusterPATStubs(t, s, now)
	stubInclusterBaoExec(t, "", "") // kv get: not seeded

	var putPath string
	var putFields map[string]string
	prevPut := baoKVPutFn
	baoKVPutFn = func(path string, fields map[string]string) error {
		putPath, putFields = path, fields
		return nil
	}
	t.Cleanup(func() { baoKVPutFn = prevPut })

	if err := runCIMintBootstrapPAT("primary"); err != nil {
		t.Fatalf("mint-bootstrap-pat: %v", err)
	}
	if s.label != "llz-incluster-primary" {
		t.Errorf("mint label = %q", s.label)
	}
	// The narrow scope set: in-cluster consumers only — and none of the
	// broad provisioning scopes.
	for _, want := range []string{"domains:read_write", "object_storage:read_write", "volumes:read_write", "linodes:read_only", "vpc:read_only", "firewall:read_write"} {
		if !strings.Contains(s.scopes, want) {
			t.Errorf("scopes missing %s: %q", want, s.scopes)
		}
	}
	for _, banned := range []string{"account:", "lke:", "nodebalancers:", "vpc:read_write"} {
		if strings.Contains(s.scopes, banned) {
			t.Errorf("scopes must not include %s: %q", banned, s.scopes)
		}
	}
	if want := linode.FmtLinodeTS(now.Unix() + 90*linode.DaySecs); s.expiry != want {
		t.Errorf("expiry = %q, want %q (90-day policy)", s.expiry, want)
	}
	if putPath != "secret/linode/api-token" {
		t.Errorf("seeded path = %q", putPath)
	}
	if putFields["token"] != "new-narrow-pat" || putFields["pat_id"] != "101" || putFields["rotated_at"] == "" {
		t.Errorf("seeded fields = %v", putFields)
	}
}

func TestMintBootstrapPATSkipsAndFailures(t *testing.T) {
	t.Run("already seeded skips the mint", func(t *testing.T) {
		inclusterPATEnv(t, "broad-pat")
		s := &patMintStub{}
		withInclusterPATStubs(t, s, time.Unix(1_800_000_000, 0))
		stubInclusterBaoExec(t, "existing-token", "")
		if err := runCIMintBootstrapPAT("primary"); err != nil {
			t.Fatalf("seeded path must skip cleanly: %v", err)
		}
		if s.patCreates != 0 {
			t.Errorf("no mint may run when the path is seeded, got %d", s.patCreates)
		}
	})

	t.Run("missing region / broad token", func(t *testing.T) {
		if err := runCIMintBootstrapPAT(""); err == nil {
			t.Error("empty region must fail")
		}
		inclusterPATEnv(t, "")
		if err := runCIMintBootstrapPAT("primary"); err == nil {
			t.Error("empty LINODE_API_TOKEN must fail")
		}
	})

	t.Run("verify failure aborts before the seed", func(t *testing.T) {
		inclusterPATEnv(t, "broad-pat")
		s := &patMintStub{stubLinode: stubLinode{verifyErr: errors.New("401")}}
		withInclusterPATStubs(t, s, time.Unix(1_800_000_000, 0))
		stubInclusterBaoExec(t, "", "")
		prevPut := baoKVPutFn
		puts := 0
		baoKVPutFn = func(string, map[string]string) error { puts++; return nil }
		t.Cleanup(func() { baoKVPutFn = prevPut })
		if err := runCIMintBootstrapPAT("primary"); err == nil || !strings.Contains(err.Error(), "verify") {
			t.Fatalf("verify failure must abort, got %v", err)
		}
		if puts != 0 {
			t.Error("an unverified token must never be seeded")
		}
	})
}

func TestRotateInclusterPATHappyPath(t *testing.T) {
	sum := inclusterPATEnv(t, "broad-pat")
	oidcServer(t)
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get pod "+rootOpenbaoPod) {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	// Real wall-clock: the drain's grace cutoff (runCredentialsPATRevokeOld)
	// uses time.Now(), not the rotatorNow seam.
	now := time.Now()
	s := &patMintStub{stubLinode: stubLinode{pats: []map[string]any{
		// An older same-labeled sibling past the 7-day grace → drained; a
		// foreign label → untouched.
		{"label": "llz-incluster-primary", "id": jn(7), "created": linode.FmtLinodeTS(now.Unix() - 30*linode.DaySecs)},
		{"label": "gha-platform-platform_LINODE_API_TOKEN", "id": jn(8), "created": linode.FmtLinodeTS(now.Unix() - 40*linode.DaySecs)},
		// The token this run just minted (newest — kept).
		{"label": "llz-incluster-primary", "id": jn(101), "created": linode.FmtLinodeTS(now.Unix())},
	}}}
	withInclusterPATStubs(t, s, now)
	calls := stubInclusterBaoExec(t, "", "propagator-token")

	if err := runCIRotateInclusterPAT(); err != nil {
		t.Fatalf("rotate-incluster-pat: %v", err)
	}
	if s.patCreates != 1 {
		t.Fatalf("patCreates = %d, want 1", s.patCreates)
	}
	if len(*calls) != 2 {
		t.Fatalf("bao exec calls = %d, want jwt login + kv put: %v", len(*calls), *calls)
	}
	login := strings.Join((*calls)[0], " ")
	if !strings.Contains(login, "auth/jwt/login") || !strings.Contains(login, "role=secret-propagator") || !strings.Contains(login, "jwt=oidc-jwt") {
		t.Errorf("login is not the expected jwt login: %v", (*calls)[0])
	}
	put := (*calls)[1]
	if put[1] != "token=propagator-token" {
		t.Errorf("kv put must use the jwt-issued token, got %s", put[1])
	}
	stdin := put[2]
	for _, want := range []string{`"token":"new-narrow-pat"`, `"pat_id":"101"`, `"rotated_at":`} {
		if !strings.Contains(stdin, want) {
			t.Errorf("kv put stdin missing %s: %s", want, stdin)
		}
	}
	for _, arg := range put[3:] {
		if strings.Contains(arg, "new-narrow-pat") {
			t.Errorf("raw token leaked onto bao argv: %v", put[3:])
		}
	}
	if len(s.deleted) != 1 || s.deleted[0] != 7 {
		t.Errorf("drain must revoke only the old same-labeled sibling, got %v", s.deleted)
	}
	if summary, _ := os.ReadFile(sum); !strings.Contains(string(summary), "new_pat_id=`101`") {
		t.Errorf("summary missing the audit line:\n%s", summary)
	}
}

func TestRotateInclusterPATSkipsAndFailures(t *testing.T) {
	// Empty broad token → hard error before anything runs.
	t.Run("empty minting token", func(t *testing.T) {
		inclusterPATEnv(t, "")
		if err := runCIRotateInclusterPAT(); err == nil {
			t.Error("empty LINODE_API_TOKEN must fail")
		}
	})

	// OpenBao pod absent → warn-skip (exit 0), nothing minted: an
	// unbootstrapped region must not accumulate orphan tokens monthly.
	t.Run("absent pod skips before the mint", func(t *testing.T) {
		inclusterPATEnv(t, "broad-pat")
		withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("NotFound") })
		s := &patMintStub{}
		withInclusterPATStubs(t, s, time.Unix(1_800_000_000, 0))
		calls := stubInclusterBaoExec(t, "", "x")
		if err := runCIRotateInclusterPAT(); err != nil {
			t.Errorf("absent pod must skip cleanly: %v", err)
		}
		if s.patCreates != 0 || len(*calls) != 0 {
			t.Errorf("skip must not mint or exec bao, got creates=%d calls=%v", s.patCreates, *calls)
		}
	})

	// jwt login failure → hard error, no kv put (the minted token is stranded
	// but same-labeled: the next run's drain collects it).
	t.Run("login failure", func(t *testing.T) {
		inclusterPATEnv(t, "broad-pat")
		oidcServer(t)
		withKubectl(t, func(string) ([]byte, error) { return nil, nil })
		s := &patMintStub{}
		withInclusterPATStubs(t, s, time.Unix(1_800_000_000, 0))
		calls := stubInclusterBaoExec(t, "", "")
		if err := runCIRotateInclusterPAT(); err == nil || !strings.Contains(err.Error(), "jwt login failed") {
			t.Errorf("login failure must error, got %v", err)
		}
		if len(*calls) != 1 {
			t.Errorf("kv put must not run after a failed login, got %v", *calls)
		}
	})

	// No id-token permission (OIDC request env unset) → hard error, no bao call.
	t.Run("no oidc permission", func(t *testing.T) {
		inclusterPATEnv(t, "broad-pat")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		withKubectl(t, func(string) ([]byte, error) { return nil, nil })
		s := &patMintStub{}
		withInclusterPATStubs(t, s, time.Unix(1_800_000_000, 0))
		calls := stubInclusterBaoExec(t, "", "x")
		if err := runCIRotateInclusterPAT(); err == nil || !strings.Contains(err.Error(), "OIDC") {
			t.Errorf("missing id-token permission must error, got %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("no bao exec before a token is minted, got %v", *calls)
		}
	})
}

func TestGithubActionsOIDCToken(t *testing.T) {
	t.Run("mints with audience + bearer", func(t *testing.T) {
		oidcServer(t)
		got, err := githubActionsOIDCToken("https://github.com/acme", nil)
		if err != nil || got != "oidc-jwt" {
			t.Fatalf("got %q, err=%v; want oidc-jwt", got, err)
		}
	})
	t.Run("errors without request env", func(t *testing.T) {
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
		if _, err := githubActionsOIDCToken("aud", nil); err == nil {
			t.Error("missing request env must error")
		}
	})
}

func TestOIDCAudienceForRepo(t *testing.T) {
	if got := oidcAudienceForRepo("acme/platform"); got != "https://github.com/acme" {
		t.Errorf("got %q", got)
	}
	if got := oidcAudienceForRepo("noslash"); got != "https://github.com/noslash" {
		t.Errorf("got %q", got)
	}
}
