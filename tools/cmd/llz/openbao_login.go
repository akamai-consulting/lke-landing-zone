package main

// openbao_login.go — `llz openbao login`, the human-operator auth primitive for
// team-scoped OpenBao writes. It runs an OAuth 2.0 Device Authorization Grant
// against the APL Keycloak realm (externally reachable at keycloak.<domainSuffix>),
// then exchanges the resulting id_token for a short-lived OpenBao token via the
// `keycloak` jwt auth mount (`llz ci bao-configure` provisions the mount + a
// per-team role bound on the Keycloak `groups` claim). The token carries only the
// team's `<name>-writer` policy, so day-2 secret writes no longer need the root
// token. See docs/designs/team-scoped-credentials.md and ADR 0004.
//
// OpenBao has no external ingress, so the id_token→token exchange rides the same
// ephemeral kubectl port-forward `get`/`set` use (portForwardOpenbaoFn).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// loginHTTPClient talks to Keycloak's OIDC endpoints. Keycloak is served on a
// real, publicly-trusted cert at keycloak.<domainSuffix>, so this is a normal
// verifying client — unlike the InsecureSkipVerify loopback client used for the
// port-forwarded OpenBao API.
var loginHTTPClient = &http.Client{Timeout: 20 * time.Second}

// oidcConfig is the subset of the OIDC discovery document the device flow needs.
type oidcConfig struct {
	DeviceEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint  string `json:"token_endpoint"`
}

// discoverOIDC fetches <issuer>/.well-known/openid-configuration and returns the
// device + token endpoints.
func discoverOIDC(hc *http.Client, issuer string) (oidcConfig, error) {
	u := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	resp, err := hc.Get(u)
	if err != nil {
		return oidcConfig{}, fmt.Errorf("OIDC discovery GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oidcConfig{}, fmt.Errorf("OIDC discovery %s: HTTP %d", u, resp.StatusCode)
	}
	var c oidcConfig
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return oidcConfig{}, fmt.Errorf("parse OIDC discovery: %w", err)
	}
	if c.DeviceEndpoint == "" || c.TokenEndpoint == "" {
		return oidcConfig{}, fmt.Errorf("OIDC discovery at %s lacks device_authorization_endpoint/token_endpoint — is the realm's OAuth 2.0 Device Flow enabled for this client?", issuer)
	}
	return c, nil
}

// deviceGrant is the device-authorization response the operator acts on.
type deviceGrant struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// startDeviceGrant opens a device-authorization request for clientID (scope
// openid, so the token response carries an id_token).
func startDeviceGrant(hc *http.Client, deviceEndpoint, clientID string) (deviceGrant, error) {
	resp, err := hc.PostForm(deviceEndpoint, url.Values{"client_id": {clientID}, "scope": {"openid"}})
	if err != nil {
		return deviceGrant{}, fmt.Errorf("device authorization POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return deviceGrant{}, fmt.Errorf("device authorization: HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var g deviceGrant
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return deviceGrant{}, fmt.Errorf("parse device authorization: %w", err)
	}
	if g.DeviceCode == "" || g.UserCode == "" {
		return deviceGrant{}, fmt.Errorf("device authorization returned no device_code/user_code")
	}
	if g.Interval <= 0 {
		g.Interval = 5 // RFC 8628 default poll interval
	}
	return g, nil
}

// pollDeviceToken polls the token endpoint until the operator finishes the
// browser login, returning the id_token. It sleeps `interval` seconds between
// polls (via the injected sleep so tests don't wait) and honors the device-flow
// authorization_pending / slow_down signals; it gives up after maxPolls.
func pollDeviceToken(hc *http.Client, tokenEndpoint, clientID, deviceCode string, interval int, sleep func(time.Duration), maxPolls int) (string, error) {
	if interval <= 0 {
		interval = 5 // defensive: never sleep(0) on authorization_pending
	}
	for i := 0; i < maxPolls; i++ {
		resp, err := hc.PostForm(tokenEndpoint, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
			"client_id":   {clientID},
		})
		if err != nil {
			// A transient blip during the multi-minute browser wait shouldn't kill
			// the login — log, back off, and keep polling (bounded by maxPolls).
			fmt.Fprintf(os.Stderr, "  (token poll retry: %v)\n", err)
			sleep(time.Duration(interval) * time.Second)
			continue
		}
		var body struct {
			IDToken   string `json:"id_token"`
			Error     string `json:"error"`
			ErrorDesc string `json:"error_description"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&body)
		status := resp.StatusCode
		resp.Body.Close()
		if derr != nil {
			return "", fmt.Errorf("parse token response (HTTP %d): %w", status, derr)
		}
		switch {
		case body.IDToken != "":
			return body.IDToken, nil
		case body.Error == "authorization_pending":
			sleep(time.Duration(interval) * time.Second)
		case body.Error == "slow_down":
			interval += 5 // RFC 8628: back off by 5s on slow_down
			sleep(time.Duration(interval) * time.Second)
		case body.Error != "":
			return "", fmt.Errorf("device login failed: %s (%s)", body.Error, body.ErrorDesc)
		default:
			return "", fmt.Errorf("token endpoint returned neither id_token nor error")
		}
	}
	return "", fmt.Errorf("timed out waiting for the browser login to complete")
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}

type openbaoLoginOpts struct {
	team     string
	region   string
	issuer   string
	clientID string
}

// keycloakIssuerForLogin derives the realm issuer from the spec (region's
// domainSuffix), with actionable errors — login is interactive, so a clear
// "pass --issuer" beats a silent skip.
func keycloakIssuerForLogin(region string) (string, error) {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return "", fmt.Errorf("could not load the spec to derive the Keycloak issuer (%w) — pass --issuer https://keycloak.<domain>/realms/otomi", err)
	}
	if region == "" {
		names := lz.EnvNames()
		if len(names) == 1 {
			region = names[0]
		} else {
			return "", fmt.Errorf("spec has %d environments (%s) — pass --region to pick one, or --issuer", len(names), strings.Join(names, ", "))
		}
	}
	e, ok := lz.Env(region)
	if !ok {
		return "", fmt.Errorf("region %q not found in the spec — pass --issuer", region)
	}
	if e.Cluster.Bootstrap.DomainSuffix == "" {
		return "", fmt.Errorf("region %q has no cluster.bootstrap.domainSuffix — pass --issuer", region)
	}
	return "https://keycloak." + e.Cluster.Bootstrap.DomainSuffix + "/realms/otomi", nil
}

func runOpenbaoLogin(o openbaoLoginOpts) error {
	if o.team == "" {
		return fmt.Errorf("--team is required (the OpenBao keycloak role, == the spec.teams name)")
	}
	clientID := firstNonEmpty(o.clientID, os.Getenv("OPENBAO_OIDC_CLIENT_ID"), "llz")
	issuer := o.issuer
	if issuer == "" {
		var err error
		if issuer, err = keycloakIssuerForLogin(o.region); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "→ Keycloak OIDC device login (issuer %s, client %s, team %s)\n", issuer, clientID, o.team)

	cfg, err := discoverOIDC(loginHTTPClient, issuer)
	if err != nil {
		return err
	}
	grant, err := startDeviceGrant(loginHTTPClient, cfg.DeviceEndpoint, clientID)
	if err != nil {
		return err
	}
	verify := firstNonEmpty(grant.VerificationURIComplete, grant.VerificationURI)
	fmt.Fprintf(os.Stderr, "\n  Open %s\n  and enter code: %s\n\n  (waiting for you to finish in the browser…)\n", verify, grant.UserCode)

	maxPolls := 60
	if grant.ExpiresIn > 0 && grant.Interval > 0 {
		maxPolls = grant.ExpiresIn/grant.Interval + 1
	}
	idToken, err := pollDeviceToken(loginHTTPClient, cfg.TokenEndpoint, clientID, grant.DeviceCode, grant.Interval, time.Sleep, maxPolls)
	if err != nil {
		return err
	}

	// Reach OpenBao over the same ephemeral port-forward get/set use, then swap
	// the id_token for a team-scoped OpenBao token via the `keycloak` mount.
	addr, cleanup, err := portForwardOpenbaoFn()
	if err != nil {
		return fmt.Errorf("port-forward to %s/%s: %w", openbaoNS, rootOpenbaoPod, err)
	}
	defer cleanup()
	token, err := openbao.OIDCLogin(context.Background(), openbao.HTTPClientInsecure(30*time.Second), addr, "keycloak", o.team, idToken)
	if err != nil {
		return err
	}
	// Deliberately NOT maskGHA(token): that writes a `::add-mask::` workflow command
	// to STDOUT, which would land in the `eval "$(…)"` output and break the shell
	// (the first line isn't valid shell). This is a local-operator eval command —
	// stdout carries only the export. Single-quote the value so a token is never
	// re-parsed by the shell (OpenBao tokens contain no single quotes).
	fmt.Fprintf(os.Stderr, "✓ authenticated as team %q — token scoped to the %s-writer policy\n", o.team, o.team)
	fmt.Fprintf(os.Stderr, "  load it into your shell:  eval \"$(llz openbao login --team %s)\"\n", o.team)
	fmt.Printf("export OPENBAO_TOKEN='%s'\n", token) // stdout only, so `eval` works
	return nil
}
