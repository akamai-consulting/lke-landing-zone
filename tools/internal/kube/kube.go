// Package kube is a minimal in-cluster Kubernetes REST client for llz
// workloads that run on the slim distroless image (no kubectl, no client-go).
// Like internal/linode it is deliberately not a general-purpose SDK; it covers
// exactly the verbs the in-cluster commands need: GET a resource, CREATE one,
// and merge-patch one.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is an authenticated Kubernetes API client.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a Client against the given API base URL with a bearer
// token. httpClient may be nil (a 30s-timeout default is used). Tests point
// base at an httptest server; production callers use NewInCluster.
func NewClient(base, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimSuffix(base, "/"), token: token, http: httpClient}
}

// NewInCluster builds a Client from the standard in-pod environment: the
// KUBERNETES_SERVICE_HOST/_PORT env vars plus the mounted ServiceAccount token
// and CA bundle (/var/run/secrets/kubernetes.io/serviceaccount/). SA_TOKEN_FILE
// / SA_CA_FILE override the mount paths — the same seam the OpenBao
// Kubernetes-auth logins use for the token.
func NewInCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST/_PORT not set — not running in a pod")
	}
	tokenFile := os.Getenv("SA_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token: %w", err)
	}
	caFile := os.Getenv("SA_CA_FILE")
	if caFile == "" {
		caFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ServiceAccount ca.crt contains no usable certificates")
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	return NewClient("https://"+host+":"+port, strings.TrimSpace(string(token)), httpClient), nil
}

// do performs one API request and returns the response body + HTTP status.
// A transport failure is the only error; non-2xx statuses are returned to the
// caller to branch on (404 → create, etc.).
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	return out, resp.StatusCode, nil
}

// GetJSON GETs an API path and unmarshals the object. A 404 yields
// (nil, 404, nil) so callers can branch; any other non-2xx is an error.
func (c *Client) GetJSON(ctx context.Context, path string) (map[string]any, int, error) {
	body, status, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, status, err
	}
	if status == http.StatusNotFound {
		return nil, status, nil
	}
	if status < 200 || status >= 300 {
		return nil, status, fmt.Errorf("GET %s returned %d: %s", path, status, truncate(body))
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, status, fmt.Errorf("GET %s: parsing response: %w", path, err)
	}
	return obj, status, nil
}

// CreateJSON POSTs an object to a collection path (e.g.
// /api/v1/namespaces/kube-system/configmaps). A 409 AlreadyExists yields
// (409, nil) — callers treat it as lost-the-race-but-fine.
func (c *Client) CreateJSON(ctx context.Context, path string, obj any) (int, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return 0, err
	}
	out, status, err := c.do(ctx, http.MethodPost, path, "application/json", body)
	if err != nil {
		return status, err
	}
	if status == http.StatusConflict {
		return status, nil
	}
	if status < 200 || status >= 300 {
		return status, fmt.Errorf("POST %s returned %d: %s", path, status, truncate(out))
	}
	return status, nil
}

// MergePatch PATCHes a resource path with an application/merge-patch+json body.
func (c *Client) MergePatch(ctx context.Context, path string, patch any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	out, status, err := c.do(ctx, http.MethodPatch, path, "application/merge-patch+json", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("PATCH %s returned %d: %s", path, status, truncate(out))
	}
	return nil
}

// WatchEvent is one frame of a Kubernetes watch stream: an event type plus the
// object it concerns. Type is ADDED | MODIFIED | DELETED | BOOKMARK | ERROR (an
// ERROR frame's Object is a metav1.Status — e.g. a 410 Gone when the requested
// resourceVersion has aged out and the caller must relist).
type WatchEvent struct {
	Type   string         `json:"type"`
	Object map[string]any `json:"object"`
}

// Watch opens a single watch stream on a list path (e.g.
// /apis/argoproj.io/v1alpha1/applications) and calls fn for every frame until the
// server closes the stream (returns nil), fn returns an error (returned as-is),
// or ctx is cancelled (returns ctx.Err()). resourceVersion may be "" to begin
// from the current state.
//
// This is the low-level primitive: it does NOT resume across a server-side close
// or relist on a 410 — a server may end a watch at any time, so the caller owns
// the outer loop (list → Watch from the list's resourceVersion → on clean return,
// relist + watch again; on a 410 ERROR frame, relist from scratch).
//
// It deliberately does not use the Client's shared *http.Client: that carries a
// 30s Timeout (NewInCluster / the NewClient default) which would guillotine a
// long-lived watch. Watch borrows only the transport (so the in-cluster CA / test
// TLS still apply) and lets ctx govern the stream's lifetime instead.
func (c *Client) Watch(ctx context.Context, path, resourceVersion string, fn func(WatchEvent) error) error {
	q := url.Values{}
	q.Set("watch", "true")
	if resourceVersion != "" {
		q.Set("resourceVersion", resourceVersion)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+sep+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	watchClient := &http.Client{Transport: c.http.Transport} // no Timeout; ctx is the deadline
	resp, err := watchClient.Do(req)
	if err != nil {
		return fmt.Errorf("watch %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("watch %s returned %d: %s", path, resp.StatusCode, truncate(body))
	}

	// The watch body is a stream of concatenated JSON WatchEvent objects; a
	// json.Decoder reads them one at a time, blocking for more until the server
	// closes (EOF) or ctx cancellation unblocks the read with an error.
	dec := json.NewDecoder(resp.Body)
	for {
		var ev WatchEvent
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("watch %s: decoding stream: %w", path, err)
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// truncate bounds an error-payload echo to keep messages readable.
func truncate(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
