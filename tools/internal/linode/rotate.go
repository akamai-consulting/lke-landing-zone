package linode

// This file extends the minimal client with the credential-rotation endpoints
// shared by the `llz credentials` (PAT + OBJ-key rotation) and secret-rotation
// commands: profile-token (PAT) CRUD, Object Storage key CRUD, and the
// LKE-Enterprise lke-admin rotation (cluster lookup + delete-kubeconfig). The
// chrono-free civil-date helpers (ported from Howard Hinnant) live here too so
// the rotators format/parse Linode timestamps identically.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DaySecs is one day in seconds — the unit the rotators reason about validity
// and grace windows in.
const DaySecs int64 = 86_400

// ── Generic HTTP helpers (POST / DELETE; GET + paginated GET) ────────────────

func (c *Client) post(ctx context.Context, url string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	return resp, nil
}

func (c *Client) del(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DELETE %s: %w", url, err)
	}
	return resp, nil
}

// listAllPages GETs every page of a Linode collection (page_size=100), returning
// the concatenated `data` arrays with numbers preserved as json.Number. `path`
// is an absolute API path beginning with `/` (e.g. `/v4/profile/tokens`).
func (c *Client) listAllPages(ctx context.Context, path string) ([]map[string]any, error) {
	var out []map[string]any
	for page := 1; ; page++ {
		url := fmt.Sprintf("%s%s?page=%d&page_size=100", c.base, path, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", url, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			text := readBody(resp)
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s returned %d (check the PAT scope): %s", path, resp.StatusCode, text)
		}
		var body struct {
			Data  []map[string]any `json:"data"`
			Pages int64            `json:"pages"`
		}
		dec := json.NewDecoder(resp.Body)
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("parsing %s response: %w", path, err)
		}
		resp.Body.Close()
		out = append(out, body.Data...)
		if body.Pages == 0 {
			body.Pages = 1
		}
		if int64(page) >= body.Pages {
			break
		}
	}
	return out, nil
}

// postJSON POSTs a JSON body to an absolute Linode URL and decodes the 2xx
// response into a generic map (numbers as json.Number). A non-2xx status is
// returned as an error that includes the response body.
func (c *Client) postJSON(ctx context.Context, url string, body any) (map[string]any, error) {
	resp, err := c.post(ctx, url, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("parsing %s response: %w", url, err)
	}
	return out, nil
}

func (c *Client) deleteExpect2xx(ctx context.Context, url, what string) error {
	resp, err := c.del(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: DELETE %s returned %d: %s", what, url, resp.StatusCode, readBody(resp))
	}
	return nil
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

// ── Profile tokens (Linode PATs) ─────────────────────────────────────────────

// ListProfileTokens returns every Personal Access Token on the account profile.
func (c *Client) ListProfileTokens(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/profile/tokens")
}

// CreateProfileToken mints a new PAT. `expiry` is a Linode-API timestamp
// (use FmtLinodeTS). The returned map includes `id` and the one-time `token`.
func (c *Client) CreateProfileToken(ctx context.Context, label, scopes, expiry string) (map[string]any, error) {
	body := map[string]any{"label": label, "scopes": scopes, "expiry": expiry}
	return c.postJSON(ctx, c.base+"/v4/profile/tokens", body)
}

// DeleteProfileToken revokes the PAT with the given ID.
func (c *Client) DeleteProfileToken(ctx context.Context, id uint64) error {
	return c.deleteExpect2xx(ctx, fmt.Sprintf("%s/v4/profile/tokens/%d", c.base, id), fmt.Sprintf("revoking PAT id=%d", id))
}

// Verify confirms a token authenticates by GETting /v4/profile (readable by any
// valid token). Used by the in-cluster rotator after a fresh mint, BEFORE the
// prior credential is drained — so a bad mint can never break a consumer.
func (c *Client) Verify(ctx context.Context) error {
	resp, err := c.get(ctx, "v4", "profile")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("token verify (GET /v4/profile): HTTP %d", resp.StatusCode)
	}
	return nil
}

// ── Object Storage keys ──────────────────────────────────────────────────────

// ListObjectStorageKeys returns every Object Storage key on the account.
func (c *Client) ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/object-storage/keys")
}

// CreateObjectStorageKey mints a new bucket-scoped key. The returned map
// includes `id`, `access_key`, and the one-time `secret_key`.
func (c *Client) CreateObjectStorageKey(ctx context.Context, label, cluster, bucket, permissions string) (map[string]any, error) {
	body := map[string]any{
		"label": label,
		"bucket_access": []any{map[string]any{
			"cluster":     cluster,
			"bucket_name": bucket,
			"permissions": permissions,
		}},
	}
	return c.postJSON(ctx, c.base+"/v4/object-storage/keys", body)
}

// DeleteObjectStorageKey revokes the OBJ key with the given ID.
func (c *Client) DeleteObjectStorageKey(ctx context.Context, id uint64) error {
	return c.deleteExpect2xx(ctx, fmt.Sprintf("%s/v4/object-storage/keys/%d", c.base, id), fmt.Sprintf("revoking OBJ key id=%d", id))
}

// ── Object Storage clusters + buckets ────────────────────────────────────────

// ListObjectStorageClusters returns every OBJ cluster on the account. Each map
// carries `id` (e.g. "us-ord-1"), `region` (e.g. "us-ord"), `domain`
// (the S3 endpoint host, e.g. "us-ord-1.linodeobjects.com"), and `status`.
func (c *Client) ListObjectStorageClusters(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/object-storage/clusters")
}

// CreateObjectStorageBucket creates a bucket in the given cluster. The returned
// map includes `label`, `cluster`, and `hostname` (<label>.<cluster>...). A
// 2xx is also returned for an already-owned bucket of the same name, so this is
// effectively idempotent for the caller's own buckets.
func (c *Client) CreateObjectStorageBucket(ctx context.Context, cluster, label string) (map[string]any, error) {
	body := map[string]any{"cluster": cluster, "label": label}
	return c.postJSON(ctx, c.base+"/v4/object-storage/buckets", body)
}

// ── LKE (lke-admin rotation) ─────────────────────────────────────────────────

// ClusterK8sVersion returns the `k8s_version` of an LKE cluster. On
// LKE-Enterprise this carries the `+lke` suffix the rotation guardrail checks.
func (c *Client) ClusterK8sVersion(ctx context.Context, clusterID uint64) (string, error) {
	resp, err := c.get(ctx, "v4beta", fmt.Sprintf("lke/clusters/%d", clusterID))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetching LKE cluster %d returned %d (check the token is valid and has LKE access): %s",
			clusterID, resp.StatusCode, readBody(resp))
	}
	var body struct {
		K8sVersion string `json:"k8s_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("parsing LKE cluster response: %w", err)
	}
	return body.K8sVersion, nil
}

// DeleteKubeconfig rotates lke-admin-token by deleting the cluster's kubeconfig,
// which invalidates and regenerates it. The lke-admin-token Secret is never
// deleted directly (per the LKE-Enterprise guidelines).
func (c *Client) DeleteKubeconfig(ctx context.Context, clusterID uint64) error {
	return c.deleteExpect2xx(ctx, fmt.Sprintf("%s/v4/lke/clusters/%d/kubeconfig", c.base, clusterID),
		"rotating lke-admin via delete-kubeconfig")
}
