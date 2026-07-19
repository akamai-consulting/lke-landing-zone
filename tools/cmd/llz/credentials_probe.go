package main

// credentials_probe.go holds the credential-probing primitives shared by the
// token-inventory writer (ci_token_inventory.go) and token validation
// (token_validate.go).
//
// These used to live inside `llz ci gh-pat-expiry` and `llz ci cred-audit` — the
// per-provider expiry probe verbs. Those verbs were superseded by the
// credential single-pane flow (token-inventory measures every CI token and
// writes the ConfigMap the in-cluster reconciler re-exposes as
// llz_token_expiry_* metrics; alert-eval reports it from the cluster) and were
// retired once they had zero callers. The MEASUREMENT primitives survived the
// verbs, so they were lifted here rather than deleted with them.

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// ghPATProbe performs one authenticated request and returns the HTTP status
// (0 == unreachable) and the raw token-expiration header. Package var so callers
// are exercisable without network access.
var ghPATProbe = func(api, token string) (code int, expHeader string, err error) {
	url := strings.TrimRight(api, "/") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err // unreachable — code 0
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("GitHub-Authentication-Token-Expiration"), nil
}

// patTarget is one service PAT to self-check: its display name, the API base to
// probe, and the token value (empty when the secret isn't set).
type patTarget struct {
	name  string
	api   string
	token string
}

// credLister is the read-only slice of the Linode client the token inventory
// needs. Injecting it lets the PAT expiry policy logic run against canned
// responses.
type credLister interface {
	ListProfileTokens(ctx context.Context) ([]map[string]any, error)
	ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error)
}
