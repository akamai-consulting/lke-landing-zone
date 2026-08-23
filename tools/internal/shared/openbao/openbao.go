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
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/portfwd"
)

// Client targets one regional OpenBao cluster.
// BaoStore is the read/write surface a caller needs from this client, and the
// seam a test replaces. IT CAME FROM internal/extensions/credrotate, which
// declared it because the rotator was the first thing to want it -- and which
// meant every other caller reached through a rotation package for a two-method
// interface over the client sitting right here.
type BaoStore interface {
	Get(ctx context.Context, path, key string) (string, bool, error)
	Write(ctx context.Context, path string, data map[string]string) error
}

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

// HTTPClientLoopback builds an *http.Client that skips TLS verification. It is
// ONLY for the loopback cases where there is nothing to verify: the `kubectl
// port-forward` tunnel `llz openbao get/set/login` open to 127.0.0.1, and the
// in-pod callers that reach OpenBao's own 127.0.0.1:8210 listener.
//
// It is NOT a general in-cluster client. Anything reaching OpenBao over the pod
// network must use HTTPClientMTLSFromFiles: the listener now requires and
// verifies a client certificate (tls_require_and_verify_client_cert), so an
// unverified transport fails the handshake outright.
//
// This was HTTPClientInsecure. #358 had already narrowed its ROLE to the
// loopback cases without renaming it; the name is what made the pod-network use
// look as sanctioned as the loopback one, so it is renamed here to match the
// role. The CA-distribution problem that originally justified the insecure
// default is long solved — each consumer namespace issues its own bundle from
// the `openbao-ca` ClusterIssuer (platform-apl/components/*/openbao-ca-bundle.yaml)
// and cert-manager writes `ca.crt` onto the resulting Secret.
func HTTPClientLoopback(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}, //nolint:gosec
	}
}

// HTTPClientMTLS builds an *http.Client that both verifies OpenBao's serving
// cert against caPEM AND presents certPEM/keyPEM as its own client identity —
// the mutually-authenticated transport every pod-network caller uses.
//
// caPEM is the openbao-ca root (ca.crt off the openbao-tls Secret); certPEM/
// keyPEM are a leaf issued by llz-client-ca, which is what the listener's
// tls_client_ca_file trusts. The two roots are deliberately different: see
// platform-apl/components/certManagerBootstrapCA/llz-client-ca.yaml.
//
// A caller that gets this wrong fails at the TLS handshake with a bare
// "remote error: tls: bad certificate" and no indication of which side was
// unhappy, so the two failure modes are separated here into distinct errors.
func HTTPClientMTLS(caPEM, certPEM, keyPEM []byte, timeout time.Duration) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("openbao CA bundle contains no valid certificate")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}},
	}, nil
}

// HTTPClientMTLSFromFiles is HTTPClientMTLS for a LONG-LIVED process: it re-reads
// the client keypair from disk on every TLS handshake instead of capturing it
// once.
//
// WHY THIS EXISTS, and why the byte-slice version is not enough for the
// reconciler. cert-manager renews the client leaf 30 days before its 90-day
// expiry and kubelet updates the mounted file within about a minute. A client
// built from bytes read at process start keeps using the ORIGINAL keypair for
// the life of the process — so roughly 90 days after the pod started, the cert
// it is still presenting expires and every OpenBao call fails the handshake.
// Nothing recovers from that on its own: the reconciler's liveness probe only
// reports leader-election health and never touches OpenBao, so the pod is never
// restarted and simply loses OpenBao access permanently.
//
// GetClientCertificate is called per handshake, not per request, and connections
// are pooled — so on a steady-state reconciler this is a handful of file reads
// per hour, not per call.
//
// Short-lived callers (the CronJobs, CI one-shots) can use either; their process
// does not outlive a renewal window. This one is correct for both, so it is the
// default in openbao_k8s_login.go.
func HTTPClientMTLSFromFiles(caFile, certFile, keyFile string, timeout time.Duration) (*http.Client, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read OpenBao CA (%s): %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("openbao CA bundle %s contains no valid certificate", caFile)
	}
	// Load once up front purely to FAIL FAST: a missing or malformed keypair
	// should be an error at startup with a clear message, not an opaque
	// handshake failure on the first OpenBao call minutes later.
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, fmt.Errorf("client keypair (%s / %s): %w", certFile, keyFile, err)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				c, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err != nil {
					return nil, fmt.Errorf("reload client keypair: %w", err)
				}
				return &c, nil
			},
		}},
	}, nil
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

// authLogin exchanges a JWT for an OpenBao client token via a JWT-shaped auth
// method (POST /v1/auth/<mount>/login {role, jwt}) and returns the issued
// client_token. Unauthenticated by design — the JWT is the credential — so it
// takes a bare *http.Client + addr rather than a *Client.
//
// `label` names the auth method in every error ("kubernetes auth login", "jwt
// auth login"), and `hint` is appended to the no-token error when a method has
// actionable guidance for that case. KubernetesLogin and JWTLogin below were
// line-for-line identical apart from those two strings and the mount segment.
func authLogin(ctx context.Context, httpClient *http.Client, addr, mount, label, role, jwt, hint string) (string, error) {
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
		return "", fmt.Errorf("%s (role %s): %w", label, role, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s (role %s): HTTP %d: %s", label, role, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("parse %s response: %w", label, err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("%s (role %s) returned no client_token%s", label, role, hint)
	}
	return out.Auth.ClientToken, nil
}

// KubernetesLogin exchanges a Kubernetes ServiceAccount JWT for an OpenBao token
// via the kubernetes auth method (POST /v1/auth/<mount>/login {role, jwt}) and
// returns the issued client_token.
func KubernetesLogin(ctx context.Context, httpClient *http.Client, addr, mount, role, jwt string) (string, error) {
	return authLogin(ctx, httpClient, addr, mount, "kubernetes auth login", role, jwt, "")
}

// JWTLogin exchanges a GitHub Actions OIDC JWT for an OpenBao client token via
// the `jwt` auth method's `auth/jwt/login` (role configured by
// `llz ci bao-configure`). It is the direct-HTTP counterpart to KubernetesLogin
// for a workload that can reach OpenBao's API over the network — an in-cluster
// runner hitting the ClusterIP — and is the auth primitive behind the secretless
// day-2 thin-caller pattern (docs/designs/cross-org-reuse-pattern.md).
func JWTLogin(ctx context.Context, httpClient *http.Client, addr, role, jwt string) (string, error) {
	return authLogin(ctx, httpClient, addr, "jwt", "jwt auth login", role, jwt,
		" — check the role's bound_claims/bound_audiences match this repo (llz ci bao-configure)")
}

// OIDCLogin exchanges a human's Keycloak (OIDC) id_token for an OpenBao client
// token via a jwt-shaped auth method at `mount` (`llz ci bao-configure` mounts
// the Keycloak one at `keycloak`). It is the human-operator counterpart to
// JWTLogin, used by `llz openbao login` so day-2 secret writes no longer need
// the root token; the issued token carries the team's `<name>-writer` policy.
func OIDCLogin(ctx context.Context, httpClient *http.Client, addr, mount, role, jwt string) (string, error) {
	return authLogin(ctx, httpClient, addr, mount, "oidc auth login", role, jwt,
		" — check the role's bound_claims (groups) match your Keycloak group (llz ci bao-configure)")
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

// MetadataList returns the immediate child keys of a KV v2 collection path (the
// LIST verb on secret/metadata/<path>), for the callers whose credential names
// are declared per deployment rather than fixed in code — the Managed Postgres
// admin paths under secret/infra/db-admin. ok=false on 404, which KV v2
// returns for an empty or never-written collection; that is "nothing declared
// here", not an error.
//
// Only leaf keys are returned. KV v2 marks a nested collection with a trailing
// slash, and nothing under this platform's listed collections nests, so a
// trailing-slash entry would be a folder the caller cannot read metadata for —
// skipped rather than passed on to become a spurious 403.
func (c *Client) MetadataList(ctx context.Context, path string) ([]string, bool, error) {
	resp, err := c.do(ctx, "LIST", MetadataPath(path), nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("list %s: HTTP %d: %s", path, resp.StatusCode, respBody(resp))
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("parse list %s: %w", path, err)
	}
	keys := make([]string, 0, len(out.Data.Keys))
	for _, k := range out.Data.Keys {
		if !strings.HasSuffix(k, "/") {
			keys = append(keys, k)
		}
	}
	return keys, true, nil
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
//
// ─────────────────────────────────────────────────────────────────────────────
// EVERY REQUEST HERE CHECKS ITS STATUS, AND NONE OF THEM USED TO. This function
// is only ever called on a path that is already half-written — DualWrite's
// secondary failed, the primary holds the NEW credential, and this is the call
// that puts the OLD one back. A rollback that reports success without having
// happened is therefore not a degraded outcome, it is the outcome the caller
// most needs to be told about, announced as the one it was hoping for.
//
// c.do returns (*http.Response, error) and the error is TRANSPORT ONLY: a 403
// from an expired token, a 429 from a rate limit, a 503 from a sealed node all
// arrive as err == nil with a status nobody read. Two consequences, and the
// second is worse than the first:
//
//   - the restore POST returning 403 made Rollback return nil, so DualWrite
//     reported "primary rolled back to v7" while the primary still served the
//     new credential and the secondary the old one — a split nothing else
//     detects, because both regions read back fine on their own.
//
//   - the prior-version GET returning an error DECODES CLEANLY into kvResponse.
//     OpenBao's error body is `{"errors":[…]}`, which has no `data` key, so
//     kv.Data.Data is left nil and the restore then POSTs {"data":null} — over
//     a LIVE SECRET, creating a new version whose content is nothing. The
//     rollback destroys the credential it was invoked to preserve.
//
// So the GET's emptiness is refused as well as its status: a version that reads
// back with no data is not something to write anywhere. readKV two hundred lines
// up has checked status since it was written; this path is the same wire, and
// the only reason it diverged is that it was added later and reached for c.do
// directly.
// ─────────────────────────────────────────────────────────────────────────────
func (c *Client) Rollback(ctx context.Context, path string, priorVersion int) error {
	if priorVersion == 0 {
		// NOT best-effort any more. This branch DESTROYS the path and every
		// version under it (KV v2 metadata delete), and a caller told the removal
		// succeeded will not come back to check. A 403 here leaves the new
		// credential live on the primary while DualWrite's message says it was
		// withdrawn.
		resp, err := c.do(ctx, http.MethodDelete, MetadataPath(path), nil)
		if err != nil {
			return fmt.Errorf("rollback %s: deleting metadata: %w", path, err)
		}
		defer resp.Body.Close()
		// 404 is the goal state reached by another route: nothing is there, which
		// is what deleting it was for.
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("rollback %s: deleting metadata: HTTP %d: %s", path, resp.StatusCode, respBody(resp))
		}
		return nil
	}

	prior, err := c.readVersion(ctx, path, priorVersion)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"data": prior})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, DataPath(path), body)
	if err != nil {
		return fmt.Errorf("rollback %s: restoring v%d: %w", path, priorVersion, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rollback %s: restoring v%d: HTTP %d: %s",
			path, priorVersion, resp.StatusCode, respBody(resp))
	}
	return nil
}

// readVersion GETs one specific version's data map, refusing everything that is
// not an answer. Separate from readKV because readKV reads the CURRENT version
// and treats 404 as "absent, not an error" — correct there, and wrong here: a
// version the caller just measured and is now restoring cannot be missing, and
// treating it as absent would send an empty map to the writer.
func (c *Client) readVersion(ctx context.Context, path string, version int) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?version=%d", DataPath(path), version), nil)
	if err != nil {
		return nil, fmt.Errorf("rollback %s: reading v%d: %w", path, version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rollback %s: reading v%d: HTTP %d: %s",
			path, version, resp.StatusCode, respBody(resp))
	}
	var kv kvResponse
	if err := json.NewDecoder(resp.Body).Decode(&kv); err != nil {
		return nil, fmt.Errorf("rollback %s: parsing v%d: %w", path, version, err)
	}
	if len(kv.Data.Data) == 0 {
		// A 2xx whose body carries no data. Either the version was destroyed or
		// the response is not the shape we think it is; writing what we decoded
		// would put an empty secret over a live one, which is the failure this
		// whole function exists to prevent.
		return nil, fmt.Errorf("rollback %s: v%d read back with no data — refusing to restore an empty secret over the live one", path, version)
	}
	return kv.Data.Data, nil
}

// DualWrite transactionally writes data to both regions. Error semantics mirror
// secret-set.sh's exit codes: primary failure leaves no change; a secondary
// failure rolls the primary back to its prior version; a post-write hash
// mismatch is flagged for manual intervention.
func DualWrite(ctx context.Context, primary, secondary *Client, path string, data map[string]string) error {
	// The error is NOT discardable. CurrentVersion returns (0, nil) when the
	// secret genuinely does not exist, and (0, err) when it could not be read —
	// a transport blip, a 403, an undecodable body. Rollback treats priorVersion
	// 0 as "there was nothing here before" and DELETES the metadata path, which
	// in KV v2 destroys the secret AND EVERY VERSION.
	//
	// So discarding this error meant: read blips → secondary write fails →
	// rollback permanently destroys a live credential it was supposed to restore.
	// A dual write that cannot establish the prior state must not begin; failing
	// here preserves the documented "primary failure leaves no change" contract.
	priorP, verErr := primary.CurrentVersion(ctx, path)
	if verErr != nil {
		return fmt.Errorf("could not read the current version of %s (no change made) — refusing to dual-write, "+
			"because a rollback could not tell 'no prior secret' from 'could not read it' and would DELETE the path: %w", path, verErr)
	}

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

// ExecArgv builds the kubectl-exec argv for the in-pod `bao` CLI, which reaches
// OpenBao over its loopback listener. It came across from the extension's cli.go
// because it is the same wire layer by another transport -- everything it needs
// (baoread.Namespace, baoread.LoopbackEnv) is already shared substrate, and
// health-sla imported the whole OpenBao capability for this one argv builder.
func ExecArgv(pod, token string, args []string) []string {
	argv := []string{"-n", baoread.Namespace, "exec", "-i", "-c", "openbao", pod, "--", "env"}
	argv = append(argv, baoread.LoopbackEnv()...)
	// Both names, same reason as the address above. The chart does not set
	// BAO_TOKEN today, so VAULT_TOKEN alone happens to work — but it works by
	// luck, and the shadowing rule is the same one that broke the address.
	argv = append(argv, "BAO_TOKEN="+token, "VAULT_TOKEN="+token, "bao")
	return append(argv, args...)
}

// ── ADDRESSING A DEPLOYMENT'S OpenBao ────────────────────────────────────────
//
// NewClientFor and ClientForward came across with the rest of the client. They
// were the last thing holding `database` to the OpenBao extension, and the only
// reason they had stayed behind is that ClientForward reads envtopology.RoleActive
// -- which was itself an extension until the same sweep moved the HA topology
// model down here. Two packages each waiting on the other to become substrate is
// the shape this tree keeps finding: a set that measures badly is usually not
// entangled with the code it names, it is waiting on a layer nobody has separated.

// NewClientFor builds a *Client for an HA role from the OPENBAO_* env. Pure
// (env → client, no side effects); the auto port-forward default lives in
// ClientForward, which callers use.
//
// The doc used to open "Client builds…", which was this function's name before the
// Client TYPE took it — so the first line named a different, existing symbol.
func NewClientFor(role string) (*Client, error) {
	var addr, token string
	switch role {
	case envtopology.RoleActive:
		addr, token = os.Getenv("OPENBAO_ADDR_ACTIVE"), firstNonEmpty(os.Getenv("OPENBAO_TOKEN_ACTIVE"), os.Getenv("OPENBAO_TOKEN"))
	case envtopology.RoleStandby:
		addr, token = os.Getenv("OPENBAO_ADDR_STANDBY"), firstNonEmpty(os.Getenv("OPENBAO_TOKEN_STANDBY"), os.Getenv("OPENBAO_TOKEN"))
	default:
		return nil, fmt.Errorf("role must be 'active' or 'standby'; got %q", role)
	}
	if addr == "" {
		return nil, fmt.Errorf("OPENBAO_ADDR_%s is not set", strings.ToUpper(role))
	}
	if token == "" {
		return nil, fmt.Errorf("OPENBAO_TOKEN_%s (or OPENBAO_TOKEN) is not set — mint a team-scoped token with `eval \"$(llz openbao login --team <name>)\"`", strings.ToUpper(role))
	}
	return New(addr, token, os.Getenv("OPENBAO_NAMESPACE"), 30*time.Second), nil
}

// ClientForward is Client plus the auto port-forward default. It
// returns a cleanup func the caller MUST defer (a no-op unless a port-forward was
// opened). When OPENBAO_ADDR_<role> is set it delegates to Client
// verbatim. Otherwise — only for the active role of a standalone deployment — it
// opens a port-forward and builds an insecure (loopback) client. A standby, or an
// active with a standby configured (an HA pair the operator addresses
// explicitly), keeps the plain env behavior and its "not set" error.
func ClientForward(role string) (*Client, func(), error) {
	noop := func() {}
	// An explicitly set address always wins — CI, HA, or a deliberate override.
	if os.Getenv("OPENBAO_ADDR_"+strings.ToUpper(role)) != "" {
		c, err := NewClientFor(role)
		return c, noop, err
	}
	// Auto-forward only the active cluster of a standalone deployment; anything
	// else keeps Client's explicit-addressing contract (and error text).
	if role != envtopology.RoleActive || StandbyConfigured() {
		c, err := NewClientFor(role)
		return c, noop, err
	}
	// The port-forward supplies the address, never the token. Accept
	// OPENBAO_ROOT_TOKEN too: `llz openbao regen-root` → export it → seed is the
	// documented operator flow, so it should work with no extra env — but a
	// team-scoped token (`llz openbao login --team`) is preferred for day-2
	// reads/writes, so warn when only the root token is present.
	token := firstNonEmpty(os.Getenv("OPENBAO_TOKEN_ACTIVE"), os.Getenv("OPENBAO_TOKEN"))
	if token == "" {
		if rt := os.Getenv("OPENBAO_ROOT_TOKEN"); rt != "" {
			WarnRootToken()
			token = rt
		}
	}
	if token == "" {
		return nil, noop, fmt.Errorf("no OpenBao token in env: set OPENBAO_TOKEN from `eval \"$(llz openbao login --team <name>)\"` (team-scoped, preferred) or export OPENBAO_ROOT_TOKEN — auto port-forward supplies the address but not the token")
	}
	addr, cleanup, err := PortForwardFn()
	if err != nil {
		return nil, noop, fmt.Errorf("auto port-forward to %s/%s: %w", baoread.Namespace, baoread.RootPod, err)
	}
	fmt.Fprintf(os.Stderr, "→ OPENBAO_ADDR_ACTIVE unset; port-forwarding %s/%s → %s (TLS verify skipped on loopback)\n", baoread.Namespace, baoread.RootPod, addr)
	c := NewWithClient(addr, token, os.Getenv("OPENBAO_NAMESPACE"), HTTPClientLoopback(30*time.Second))
	return c, cleanup, nil
}

// StandbyConfigured reports whether a standby cluster is addressable — i.e. this
// is an HA pair, not a standalone deployment.
func StandbyConfigured() bool { return os.Getenv("OPENBAO_ADDR_STANDBY") != "" }

// WarnRootToken nudges an operator who supplied the OpenBao root token toward the
// team-scoped `llz openbao login` path. Root still works — this is a warning, not
// a block — but day-2 secret access should use a short-lived, attributed,
// least-privilege team token instead. Written to stderr so it never pollutes the
// value `get` prints to stdout, and suppressed when OPENBAO_ALLOW_ROOT is set (an
// escape hatch for genuine root-only automation that has no team identity).
func WarnRootToken() {
	if os.Getenv("OPENBAO_ALLOW_ROOT") != "" {
		return
	}
	fmt.Fprintln(os.Stderr, "⚠ using the OpenBao ROOT token — prefer a team-scoped token for day-2 secret access:")
	fmt.Fprintln(os.Stderr, "    eval \"$(llz openbao login --team <name>)\"   # short-lived, attributed, least-privilege")
	fmt.Fprintln(os.Stderr, "  (set OPENBAO_ALLOW_ROOT=1 to silence this for root-only automation)")
}

// PortForward is portForward, exported for the capability wiring in
// internal/cli. assert-identity's team-login smoke needs the ADDRESS (it hands it
// to OIDCLogin), not the *Client that ClientForward returns, and its Deps field
// is exactly this signature — so the alternative was a fourth copy of the
// port-forward dance in the composition root.
func PortForward() (string, func(), error) { return portForward() }

// portForward runs `kubectl port-forward` to OpenBao pod-0 on a
// kubectl-chosen local port (":0"), waits for it to be announced + the tunnel to
// warm up, and returns the https base URL and a kill/reap teardown.
func portForward() (string, func(), error) {
	// Forward to the LOOPBACK listener (8210), not the mTLS network listener
	// (8200). port-forward is established inside the pod's network namespace, so
	// a 127.0.0.1-bound port is reachable — which is what lets an operator use
	// `llz openbao get/set` from a laptop that holds no client certificate.
	cmd := exec.Command("kubectl", "port-forward", "-n", baoread.Namespace, "pod/"+baoread.RootPod, ":"+baoread.LoopbackPort)
	// Surface kubectl's own stderr live: without this the common failure modes
	// (wrong kube-context, pod-0 absent, RBAC-denied on pods/portforward) are
	// swallowed and the operator only sees an opaque establish timeout. kubectl
	// writes "Forwarding from…"/"Handling connection…" to stdout, so stderr
	// carries errors alone — no normal-path noise.
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }

	localPort, err := portfwd.ReadForwardPortTimeout(stdout, portfwd.ForwardEstablishTimeout)
	if err != nil {
		stop()
		return "", nil, err
	}
	// Keep draining stdout so kubectl's per-connection log lines can't fill the
	// pipe buffer and block its writer (same rationale as withPrometheus).
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	base := "https://127.0.0.1:" + localPort
	if err := warmUp(base); err != nil {
		stop()
		return "", nil, err
	}
	return base, stop, nil
}

// PortForwardFn is the seam a test replaces to avoid a real kubectl port-forward.
var PortForwardFn = portForward

// firstNonEmpty is a MINIMAL LOCAL COPY, not an import. It is four lines, several
// packages in this tree keep their own, and reaching for a shared one would drag a
// dependency across a layer to save nothing.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// warmUp blocks (bounded) until the tunnel answers, so the first real KV
// call doesn't race the port-forward coming up. Any HTTP response — even a
// sealed/standby non-2xx from /v1/sys/seal-status — proves the tunnel is up.
func warmUp(base string) error {
	client := HTTPClientLoopback(5 * time.Second)
	var lastErr error
	for i := 0; i < 15; i++ {
		resp, err := client.Get(base + "/v1/sys/seal-status")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port-forward tunnel never became ready: %w", lastErr)
}

// ── PARSERS FOR `bao status` / `bao token lookup` OUTPUT ─────────────────────
//
// Both came from internal/extensions/openbao, under a heading that already
// said what they were: "pure parse helpers (unit-tested)". Reading OpenBao's JSON
// is this package's subject; regenerating a root token is a lifecycle capability,
// and identity-config and reachability were each importing that whole capability
// to decode one field.

func ParseStatus(s string) (sealed bool, threshold int) {
	sealed, threshold, _ = ParseStatusOK(s)
	return sealed, threshold
}

// ParseStatusOK is ParseStatus with "there was usable JSON" reported separately.
//
// The third return is the whole point, and callers that DECIDE on the seal state
// need it. `bao status` EXITS NON-ZERO when the pod is sealed and still prints
// valid JSON — so a caller that bails on the exec error never reaches the sealed
// branch, and a caller that swallows the parse error cannot tell "sealed" from
// "nothing answered": both arrive here as the zero value, which reads as UNSEALED.
// baoread.ParsePodStatus's doc states the same rule for the same reason.
func ParseStatusOK(s string) (sealed bool, threshold int, ok bool) {
	var v struct {
		Sealed bool `json:"sealed"`
		T      int  `json:"t"`
	}
	if json.Unmarshal([]byte(s), &v) != nil {
		return false, 0, false
	}
	return v.Sealed, v.T, true
}

func PoliciesIncludeRoot(lookupJSON string) bool {
	var v struct {
		Data struct {
			Policies []string `json:"policies"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(lookupJSON), &v)
	for _, p := range v.Data.Policies {
		if p == "root" {
			return true
		}
	}
	return false
}
