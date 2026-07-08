// Package openbao is a minimal HTTP client for the OpenBao (Vault) KV v2 API,
// ported from instance-scripts/openbao/secret-{get,set}.sh. It exists so the
// transactional dual-region write — write primary, write secondary, roll the
// primary back if the secondary fails, then verify both regions hashed equal —
// is tested Go rather than re-derived in bash, and so secret values stay off the
// process argv. OpenBao OSS has no cross-region replication; this client + the
// operator-side dual-write IS the replication.
package openbao

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client targets one regional OpenBao cluster.
type Client struct {
	addr      string
	token     string
	namespace string
	http      *http.Client
}

func New(addr, token, namespace string, timeout time.Duration) *Client {
	return NewWithClient(addr, token, namespace, &http.Client{Timeout: timeout})
}

// NewWithClient is New with a caller-supplied *http.Client — used by in-cluster
// callers that need a CA-trusting transport for OpenBao's private serving cert
// (see HTTPClientWithCA) and reuse it across login + writes.
func NewWithClient(addr, token, namespace string, httpClient *http.Client) *Client {
	return &Client{
		addr:      strings.TrimRight(addr, "/"),
		token:     token,
		namespace: namespace,
		http:      httpClient,
	}
}

// HTTPClientInsecure builds an *http.Client that skips TLS verification — the
// established in-cluster posture for OpenBao access (every `baoExec` call uses
// VAULT_SKIP_VERIFY=true), since distributing OpenBao's private CA into each
// consumer namespace needs a reflector this platform doesn't ship. Pod→OpenBao
// traffic stays on the cluster pod network. Prefer HTTPClientWithCA where the CA
// is mounted.
func HTTPClientInsecure(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}, //nolint:gosec
	}
}

// HTTPClientWithCA builds an *http.Client that trusts caPEM (the openbao-ca
// bundle an in-cluster pod mounts) for TLS to OpenBao. OpenBao's serving cert is
// signed by a private CA, so the system bundle alone can't verify it.
func HTTPClientWithCA(caPEM []byte, timeout time.Duration) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificate found in CA bundle")
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}, nil
}

// KubernetesLogin exchanges a Kubernetes ServiceAccount JWT for an OpenBao token
// via the kubernetes auth method (POST /v1/auth/<mount>/login {role, jwt}) and
// returns the issued client_token. Unauthenticated by design — the JWT is the
// credential — so it takes a bare *http.Client + addr rather than a *Client.
func KubernetesLogin(ctx context.Context, httpClient *http.Client, addr, mount, role, jwt string) (string, error) {
	body, err := json.Marshal(map[string]string{"role": role, "jwt": jwt})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(addr, "/") + "/v1/auth/" + mount + "/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("kubernetes auth login (role %s): %w", role, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("kubernetes auth login (role %s): HTTP %d: %s", role, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("parse kubernetes auth login response: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("kubernetes auth login (role %s) returned no client_token", role)
	}
	return out.Auth.ClientToken, nil
}

// DataPath turns an operator KV path (secret/app/keys) into the KV v2 data API
// path (secret/data/app/keys). MetadataPath does the metadata equivalent.
func DataPath(p string) string     { return strings.Replace(p, "secret/", "secret/data/", 1) }
func MetadataPath(p string) string { return strings.Replace(p, "secret/", "secret/metadata/", 1) }

// ValidatePath requires the KV v2 `secret/` mount prefix.
func ValidatePath(p string) error {
	if !strings.HasPrefix(p, "secret/") {
		return fmt.Errorf("path must begin with 'secret/' (KV v2). got: %s", p)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, apiPath string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+apiPath, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", c.token)
	if c.namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.namespace)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// kvResponse is the shape of a KV v2 read.
type kvResponse struct {
	Data struct {
		Data     map[string]any `json:"data"`
		Metadata struct {
			Version int `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
}

// readKV GETs secret/data/<path>; ok=false on 404 (secret absent).
func (c *Client) readKV(ctx context.Context, path string) (kv kvResponse, ok bool, err error) {
	resp, err := c.do(ctx, http.MethodGet, DataPath(path), nil)
	if err != nil {
		return kv, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return kv, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return kv, false, fmt.Errorf("read %s: HTTP %d: %s", path, resp.StatusCode, respBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&kv); err != nil {
		return kv, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return kv, true, nil
}

// Get reads a single field from a secret. Returns ("", false, nil) if the key is
// absent, ("", false, err) on read failure.
func (c *Client) Get(ctx context.Context, path, key string) (string, bool, error) {
	kv, ok, err := c.readKV(ctx, path)
	if err != nil || !ok {
		return "", false, err
	}
	v, present := kv.Data.Data[key]
	if !present || v == nil {
		return "", false, nil
	}
	return fmt.Sprintf("%v", v), true, nil
}

// CurrentVersion returns the secret's current version, or 0 if it does not exist.
func (c *Client) CurrentVersion(ctx context.Context, path string) (int, error) {
	kv, ok, err := c.readKV(ctx, path)
	if err != nil || !ok {
		return 0, err
	}
	return kv.Data.Metadata.Version, nil
}

// SealInfo is the subset of /v1/sys/seal-status the reconciler reads.
type SealInfo struct {
	Sealed      bool `json:"sealed"`
	Initialized bool `json:"initialized"`
}

// SealStatus reports OpenBao's seal state. /v1/sys/seal-status is an
// unauthenticated endpoint, so this works with a tokenless client too.
func (c *Client) SealStatus(ctx context.Context) (SealInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "sys/seal-status", nil)
	if err != nil {
		return SealInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SealInfo{}, fmt.Errorf("seal-status: HTTP %d: %s", resp.StatusCode, respBody(resp))
	}
	var si SealInfo
	if err := json.NewDecoder(resp.Body).Decode(&si); err != nil {
		return SealInfo{}, fmt.Errorf("parse seal-status: %w", err)
	}
	return si, nil
}

// MetadataUpdatedTime returns when the KV v2 secret at path was last written
// (its metadata `updated_time`) — the rotation-age source the SLA checks use.
// ok=false if the secret does not exist (404). Reading metadata needs only a
// metadata-read capability, not access to the secret data.
func (c *Client) MetadataUpdatedTime(ctx context.Context, path string) (time.Time, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, MetadataPath(path), nil)
	if err != nil {
		return time.Time{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return time.Time{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return time.Time{}, false, fmt.Errorf("metadata %s: HTTP %d: %s", path, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data struct {
			UpdatedTime string `json:"updated_time"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return time.Time{}, false, fmt.Errorf("parse metadata %s: %w", path, err)
	}
	if out.Data.UpdatedTime == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, out.Data.UpdatedTime)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse updated_time %q: %w", out.Data.UpdatedTime, err)
	}
	return t, true, nil
}

// Write POSTs {data: <pairs>} to secret/data/<path>, creating a new version.
func (c *Client) Write(ctx context.Context, path string, data map[string]string) error {
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, DataPath(path), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("write %s: HTTP %d: %s", path, resp.StatusCode, respBody(resp))
	}
	return nil
}

// DataHash returns a sha256 over the canonical JSON of the secret's data map.
// Both regions canonicalize identically (Go sorts map keys), so equal content
// hashes equal regardless of OpenBao's field ordering.
func (c *Client) DataHash(ctx context.Context, path string) (string, error) {
	kv, ok, err := c.readKV(ctx, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("read-back %s: secret absent", path)
	}
	canon, err := json.Marshal(kv.Data.Data) // Go marshals map keys sorted
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return fmt.Sprintf("%x", sum), nil
}

// Rollback restores priorVersion's data as a new version. If priorVersion is 0
// (no secret existed before), it deletes the metadata so the secret is removed.
func (c *Client) Rollback(ctx context.Context, path string, priorVersion int) error {
	if priorVersion == 0 {
		resp, err := c.do(ctx, http.MethodDelete, MetadataPath(path), nil)
		if err == nil {
			resp.Body.Close()
		}
		return err // best-effort
	}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?version=%d", DataPath(path), priorVersion), nil)
	if err != nil {
		return err
	}
	var kv kvResponse
	dec := json.NewDecoder(resp.Body).Decode(&kv)
	resp.Body.Close()
	if dec != nil {
		return dec
	}
	body, err := json.Marshal(map[string]any{"data": kv.Data.Data})
	if err != nil {
		return err
	}
	r2, err := c.do(ctx, http.MethodPost, DataPath(path), body)
	if err != nil {
		return err
	}
	r2.Body.Close()
	return nil
}

// DualWrite transactionally writes data to both regions. Error semantics mirror
// secret-set.sh's exit codes: primary failure leaves no change; a secondary
// failure rolls the primary back to its prior version; a post-write hash
// mismatch is flagged for manual intervention.
func DualWrite(ctx context.Context, primary, secondary *Client, path string, data map[string]string) error {
	priorP, _ := primary.CurrentVersion(ctx, path)

	if err := primary.Write(ctx, path, data); err != nil {
		return fmt.Errorf("primary write failed (no change made): %w", err)
	}
	if err := secondary.Write(ctx, path, data); err != nil {
		if rbErr := primary.Rollback(ctx, path, priorP); rbErr != nil {
			return fmt.Errorf("secondary write failed AND primary rollback failed — MANUAL INTERVENTION for %s: write=%v rollback=%v", path, err, rbErr)
		}
		return fmt.Errorf("secondary write failed; primary rolled back to v%d: %w", priorP, err)
	}

	hp, err := primary.DataHash(ctx, path)
	if err != nil {
		return fmt.Errorf("primary read-back failed: %w", err)
	}
	hs, err := secondary.DataHash(ctx, path)
	if err != nil {
		return fmt.Errorf("secondary read-back failed: %w", err)
	}
	if hp != hs {
		return fmt.Errorf("HASH MISMATCH after write (primary=%s secondary=%s) — MANUAL INTERVENTION for %s", hp[:12], hs[:12], path)
	}
	return nil
}

func respBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}
