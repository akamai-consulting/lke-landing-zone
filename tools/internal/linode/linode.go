// Package linode is a small Linode API client used by the llz tooling — LKE
// cluster lifecycle, control-plane ACL, credential rotation, and the resource
// reaper. It is deliberately not a general-purpose SDK; it covers exactly the
// endpoints these tools need.
package linode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// APIBase is the Linode API root. Paths are versioned per-call (v4 / v4beta).
const APIBase = "https://api.linode.com"

// Client is an authenticated Linode API client.
type Client struct {
	token string
	http  *http.Client
	// base is the API root every request is built against. It defaults to
	// APIBase; tests point it at an httptest server. Keeping it a field (rather
	// than referencing the APIBase const directly) is the only seam needed to
	// exercise the request builders without reaching the real Linode API.
	base string
}

// NewClient returns a Client that authenticates with the given personal access
// token and applies the given per-request timeout.
func NewClient(token string, timeout time.Duration) *Client {
	return &Client{token: token, http: &http.Client{Timeout: timeout}, base: APIBase}
}

func (c *Client) get(ctx context.Context, version, path string) (*http.Response, error) {
	url := fmt.Sprintf("%s/%s/%s", c.base, version, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return resp, nil
}

// ListRaw GETs a Linode collection endpoint and returns its `data` array as
// generic maps (numbers preserved as json.Number), along with the HTTP status.
// The returned error is non-nil only for transport or parse failures; a non-2xx
// response yields (nil, status, nil) so the caller can branch on the status.
func (c *Client) ListRaw(ctx context.Context, version, path string) ([]map[string]any, int, error) {
	resp, err := c.get(ctx, version, path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, nil
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parsing %s response: %w", path, err)
	}
	return body.Data, resp.StatusCode, nil
}

func (c *Client) put(ctx context.Context, url string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PUT %s: %w", url, err)
	}
	return resp, nil
}
