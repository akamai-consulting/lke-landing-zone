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
	"io"
	"net/http"
	"time"
)

// APIBase is the Linode API root. Paths are versioned per-call (v4 / v4beta).
const APIBase = "https://api.linode.com"

// MethodPolicy judges one request before it is sent. It exists so the capability
// layer can police what a binding's declared grants permit WITHOUT this package
// knowing anything about grants — linode enforces, capability decides.
//
// A nil policy permits everything, and that default is deliberate in the SAFE
// direction only because every construction site inside internal/extensions is
// held to installing one by a ratchet (capability's rawcloud_test.go). The
// alternative — refusing by default — would make this package unusable from
// internal/verbs and cmd/llz, which hold no bindings by design.
type MethodPolicy func(method, url string) error

// Client is an authenticated Linode API client.
type Client struct {
	token string
	http  *http.Client
	// policy, when non-nil, is consulted before every request. See MethodPolicy.
	policy MethodPolicy
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

// WithBase returns a copy of the client pointed at a different API root.
//
// EXPORTED FOR THE FENCE'S TESTS, and worth the surface. `base` was an unexported
// field the package's own in-package tests set directly, which is fine until the
// thing that needs testing lives OUTSIDE the package — capability's cloud handle
// has to prove that a refused DELETE never reaches the wire, and that assertion is
// only meaningful against a real server. Returning a copy, like WithPolicy, so a
// retargeted client cannot be mistaken for one aimed at Linode.
func (c *Client) WithBase(base string) *Client {
	out := *c
	out.base = base
	return &out
}

// WithPolicy returns a copy of the client that consults p before every request.
// A copy rather than a mutation so a policed client can never be handed back to a
// caller expecting the unpoliced one.
func (c *Client) WithPolicy(p MethodPolicy) *Client {
	out := *c
	out.policy = p
	return &out
}

// do issues one authenticated Linode API request and returns the raw response.
// A non-nil body is JSON-marshalled and sent with Content-Type: application/json;
// a nil body sends no payload and no content type. Transport failures are wrapped
// as "<METHOD> <url>: <err>".
//
// Every verb below (and the paginated GET in rotate.go) funnels through here —
// they were previously five hand-rolled copies of the same marshal → build →
// set Authorization → Do → wrap sequence, with no divergence in headers,
// timeout, or error shape.
func (c *Client) do(ctx context.Context, method, url string, body any) (*http.Response, error) {
	// THE POLICY GATE, AND IT IS HERE FOR THE REASON THIS FUNCTION EXISTS. The
	// header above records that all fifty-two Client methods funnel through this
	// one — `c.http.Do` appears exactly once in the package — so a check here is a
	// check on every Linode request the tooling makes. Placing it on the individual
	// verbs instead would be fifty-two places to forget.
	//
	// It runs BEFORE the request is built, so a refused mutation costs no
	// marshalling and, more importantly, cannot half-happen.
	if c.policy != nil {
		if err := c.policy(method, url); err != nil {
			return nil, err
		}
	}
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, version, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, fmt.Sprintf("%s/%s/%s", c.base, version, path), nil)
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
	return c.do(ctx, http.MethodPut, url, body)
}
