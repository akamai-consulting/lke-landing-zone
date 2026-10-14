package openbao

// introspect.go — read-only questions a gate asks OpenBao about the CALLING
// token: what it may do on a path, what engine backs a mount, what a path holds.
// They exist for the spec.openbao gate (assertidentity), which logs in through
// each declared role the way the real consumer does and then checks ground truth
// rather than re-reading the policy text bao-configure wrote.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// CapabilitiesSelf returns the calling token's capabilities on each path
// (POST sys/capabilities-self). Every requested path must appear in the answer:
// a response that omits one is an error, never an empty capability set, so a gate
// cannot pass on a reply it did not understand.
func (c *Client) CapabilitiesSelf(ctx context.Context, paths []string) (map[string][]string, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("capabilities-self: no paths requested")
	}
	body, err := json.Marshal(map[string]any{"paths": paths})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "sys/capabilities-self", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("capabilities-self: HTTP %d: %s", resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse capabilities-self: %w", err)
	}
	got := make(map[string][]string, len(paths))
	for _, p := range paths {
		raw, ok := out.Data[p]
		if !ok {
			return nil, fmt.Errorf("capabilities-self: answer omits %q", p)
		}
		var caps []string
		if err := json.Unmarshal(raw, &caps); err != nil {
			return nil, fmt.Errorf("parse capabilities for %q: %w", p, err)
		}
		got[p] = caps
	}
	return got, nil
}

// MountType returns the engine type backing mount (GET
// sys/internal/ui/mounts/<mount>), which any token with a capability under the
// mount may call. ok=false on 404 (no such mount). A 403 is an error: it means
// the token holds nothing under the mount, which is itself a finding.
func (c *Client) MountType(ctx context.Context, mount string) (string, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "sys/internal/ui/mounts/"+strings.Trim(mount, "/"), nil)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("mount %s: HTTP %d: %s", mount, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, fmt.Errorf("parse mount %s: %w", mount, err)
	}
	if out.Data.Type == "" {
		return "", false, fmt.Errorf("mount %s: answer carries no type", mount)
	}
	// The endpoint resolves the LONGEST mount prefix of the path, so a request for
	// a mount that does not exist under a mount that does would otherwise report
	// the parent's type. Require the answer to name the mount asked about.
	if strings.Trim(out.Data.Path, "/") != strings.Trim(mount, "/") {
		return "", false, nil
	}
	return out.Data.Type, true, nil
}

// Read GETs an arbitrary API path and returns its `data` object. ok=false on 404.
func (c *Client) Read(ctx context.Context, apiPath string) (map[string]any, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("read %s: HTTP %d: %s", apiPath, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", apiPath, err)
	}
	return out.Data, true, nil
}

// TokenPolicies returns the calling token's own policies (GET
// auth/token/lookup-self, `data.policies`), which every token's `default` policy
// may read. A gate uses it for the "at most" half that capabilities-self cannot
// answer: a token carrying an UNDECLARED policy still holds every declared
// capability. An answer without a policies list is an error, never "none".
func (c *Client) TokenPolicies(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "auth/token/lookup-self", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lookup-self: HTTP %d: %s", resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data struct {
			Policies *[]string `json:"policies"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse lookup-self: %w", err)
	}
	if out.Data.Policies == nil {
		return nil, fmt.Errorf("lookup-self: answer carries no policies list")
	}
	return *out.Data.Policies, nil
}

// List LISTs an arbitrary API path and returns `data.keys`. A 404 is OpenBao's
// answer for "nothing under this path" and returns an empty list; any other
// non-2xx is an error.
func (c *Client) List(ctx context.Context, apiPath string) ([]string, error) {
	resp, err := c.do(ctx, "LIST", apiPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list %s: HTTP %d: %s", apiPath, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse list %s: %w", apiPath, err)
	}
	return out.Data.Keys, nil
}
