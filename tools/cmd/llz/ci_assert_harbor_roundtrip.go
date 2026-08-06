package main

// ci_assert_harbor_roundtrip.go implements `llz ci assert-harbor-roundtrip` — the
// gate that a minted Harbor robot can actually authenticate to the registry for
// both PULL and PUSH.
//
// THE REGRESSION IT EXISTS FOR. Managed instances rendered HARBOR_HOST as
// "harbor." — the domain suffix was empty, so the host was truncated to a
// trailing dot. Non-empty, so it defeated every `== ""` guard, including the
// systeminfo fallback that was supposed to discover the real host. The robot was
// minted correctly, published correctly, and stored correctly; every credential
// in the chain was valid. What broke was the HOST they were presented to, and the
// result was a 401 on push AND on pull. Nothing caught it, because nothing ever
// used the credential — the provisioner asserted it had CREATED a robot, not that
// the robot could log in anywhere.
//
// `usableRegistryHost` (ci_harbor_provisioner.go) is the static half of the fix
// and it is good, but it can only reject hosts that look malformed to it. This is
// the empirical half: present the credential to the registry and see.
//
// WHAT IT DOES. The OCI distribution v2 auth handshake, which is exactly what a
// `docker push` does before it moves a byte:
//
//   1. GET /v2/ unauthenticated → expect 401 with a Www-Authenticate: Bearer
//      challenge naming the token realm. (A 200 here means the registry is
//      unauthenticated, which is its own finding.)
//   2. Request a token from the realm with the robot's basic auth, for
//      scope "repository:<repo>:pull,push".
//   3. ASSERT THE TOKEN CARRIES THE REQUESTED ACCESS. This is the sharp bit:
//      Harbor's token service returns 200 with a VALID JWT and an EMPTY access
//      list when the credential lacks the scope. "I got a token" is not proof of
//      authorization, and a gate that stopped at the status code would pass for a
//      robot that can do nothing.
//   4. Use it: GET /v2/<repo>/tags/list (pull) and open + immediately cancel a
//      blob upload session, POST then DELETE (push). The upload session is the
//      cheapest operation that requires real push authorization, and cancelling
//      it leaves nothing behind.
//
// It uploads no layers and creates no tags — the push half is proven by the
// registry granting an upload session, not by pushing an artifact into a project
// that e2e would then have to clean up.
//
// FAIL-CLOSED: an unreachable registry, a missing credential Secret, a token with
// no access, or any unexpected status fails. Read-only in effect.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	// harborRobotSecretNS/Name is the Secret ESO materializes from
	// secret/harbor/robot (llz-cert-automation chart, harborDockerConfig).
	harborRobotSecretNS   = "llz-cert-automation"
	harborRobotSecretName = "harbor-docker-config"
	// harborProbeRepo is the repository the scope is requested against. It need
	// not exist: a token scoped to a repository is granted (or refused) on the
	// robot's policy, and tags/list on a missing repo is a clean NAME_UNKNOWN.
	harborProbeRepo = "library/llz-roundtrip-probe"
)

func ciAssertHarborRoundTripCmd() *cobra.Command {
	var secretNS, secretName, repo, registry string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-harbor-roundtrip",
		Short: "fail unless a minted Harbor robot can authenticate for pull AND push",
		Long: "Performs the OCI distribution v2 auth handshake with the robot credential ESO\n" +
			"materialized from secret/harbor/robot: fetch the Bearer challenge, exchange the\n" +
			"robot's basic auth for a scoped token, verify the token actually CARRIES\n" +
			"pull+push access, then exercise both (tags/list, and open+cancel a blob upload\n" +
			"session).\n\n" +
			"Managed instances once rendered HARBOR_HOST as \"harbor.\" — non-empty, so it\n" +
			"defeated every empty-string guard including the systeminfo fallback — and every\n" +
			"push and pull 401'd. Every credential in the chain was valid; the HOST was\n" +
			"wrong. Nothing caught it because nothing ever USED the credential: the\n" +
			"provisioner asserted it had created a robot, not that the robot could log in.\n\n" +
			"Harbor's token service returns 200 with a valid JWT and an EMPTY access list\n" +
			"when the credential lacks the scope, so this asserts the granted access, not the\n" +
			"status code. Uploads no layers and creates no tags. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertHarborRoundTrip(secretNS, secretName, registry, repo,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&secretNS, "secret-namespace", harborRobotSecretNS, "namespace of the robot credential Secret")
	c.Flags().StringVar(&secretName, "secret-name", harborRobotSecretName, "name of the robot credential Secret")
	c.Flags().StringVar(&registry, "registry", "", "registry host override (default: the Secret's registry_host)")
	c.Flags().StringVar(&repo, "repo", harborProbeRepo, "repository the pull+push scope is requested against")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}

// harborRobotCreds is what the round trip needs.
type harborRobotCreds struct {
	Username     string
	Password     string
	RegistryHost string
}

// decodeRobotSecret extracts the robot credential from a Secret's JSON. Pure.
//
// It rejects a registry_host that usableRegistryHost refuses, using the SAME
// predicate the provisioner uses rather than restating it — a gate that
// re-implemented "looks like a host" would drift from the guard it is backing up,
// and "harbor." passing one but not the other is precisely the bug.
func decodeRobotSecret(raw []byte) (harborRobotCreds, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return harborRobotCreds{}, fmt.Errorf("decoding the robot Secret: %w", err)
	}
	get := func(k string) (string, error) {
		v, ok := s.Data[k]
		if !ok {
			return "", fmt.Errorf("the robot Secret has no %q key — ESO has not materialized it from secret/harbor/robot", k)
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return "", fmt.Errorf("the robot Secret's %q is not valid base64: %w", k, err)
		}
		return string(b), nil
	}
	var c harborRobotCreds
	var err error
	if c.Username, err = get("username"); err != nil {
		return c, err
	}
	if c.Password, err = get("password"); err != nil {
		return c, err
	}
	if c.RegistryHost, err = get("registry_host"); err != nil {
		return c, err
	}
	if !usableRegistryHost(c.RegistryHost) {
		return c, fmt.Errorf("registry_host %q is not a usable registry host — this is the truncation class "+
			"(\"harbor.\" is non-empty, so every == \"\" guard passes it) and every push and pull will 401", c.RegistryHost)
	}
	return c, nil
}

// bearerChallenge is the parsed Www-Authenticate header.
type bearerChallenge struct {
	Realm   string
	Service string
}

// parseBearerChallenge parses a `Bearer realm="…",service="…"` header. Pure.
func parseBearerChallenge(h string) (bearerChallenge, error) {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return bearerChallenge{}, fmt.Errorf("not a Bearer challenge: %q", h)
	}
	var c bearerChallenge
	for _, part := range strings.Split(h[len("Bearer "):], ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(k) {
		case "realm":
			c.Realm = v
		case "service":
			c.Service = v
		}
	}
	if c.Realm == "" {
		return c, fmt.Errorf("bearer challenge carried no realm: %q", h)
	}
	return c, nil
}

// tokenAccess is the access grant inside a token-service response.
type tokenAccess struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// parseTokenResponse extracts the token and its granted access. Pure.
func parseTokenResponse(raw []byte) (string, []tokenAccess, error) {
	var r struct {
		Token       string        `json:"token"`
		AccessToken string        `json:"access_token"`
		Access      []tokenAccess `json:"access"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", nil, fmt.Errorf("decoding the token response: %w", err)
	}
	tok := firstNonEmpty(r.Token, r.AccessToken)
	if tok == "" {
		return "", nil, fmt.Errorf("the token service returned no token")
	}
	// THE ACCESS LIST IS IN THE JWT, NOT THE ENVELOPE. Harbor's /service/token
	// answers with {token, access_token, expires_in, issued_at} — measured; there is
	// no `access` key in the body at all, and the distribution token spec does not
	// put one there. The granted scope is a CLAIM inside the token.
	//
	// Reading r.Access therefore always yielded an empty list, so grantedActions
	// always returned nothing and every caller concluded the credential held no
	// push scope. The comment at the call site had the mechanism exactly right —
	// "Harbor returns a valid token with an empty access list when the credential
	// lacks the scope" — which is what made the empty result look like a diagnosis
	// instead of a bug: the check reported the one failure it was written to detect,
	// on every credential, including ones that could demonstrably push.
	//
	// It survived because both callers were guarded on a namespace that does not
	// exist on managed clusters, so neither ever reached this line where we run.
	//
	// The body is still preferred when it does carry an access list: a registry that
	// states the grant explicitly is more authoritative than our reading of its token.
	if len(r.Access) > 0 {
		return tok, r.Access, nil
	}
	return tok, jwtAccessClaims(tok), nil
}

// jwtAccessClaims pulls the `access` claim out of an unverified JWT payload.
//
// UNVERIFIED IS CORRECT HERE, and it is worth being explicit about why. We are not
// making an authorization decision — the registry does that when the token is
// presented, and the push either succeeds or 401s. This only reads what the token
// SAYS it grants, to turn a coming 401 into a message that names the missing scope.
// A forged token would fail at the registry regardless; verifying the signature here
// would mean fetching Harbor's signing key to improve an error string.
//
// Returns nil for anything unparseable: the caller then reports no granted actions,
// which is the same conservative answer as before.
func jwtAccessClaims(token string) []tokenAccess {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	// JWT uses base64url WITHOUT padding; RawURLEncoding is the matching decoder.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Access []tokenAccess `json:"access"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims.Access
}

// grantedActions returns the actions granted for a repository across the access
// list. Pure.
func grantedActions(access []tokenAccess, repo string) map[string]bool {
	out := map[string]bool{}
	for _, a := range access {
		if a.Name != repo {
			continue
		}
		for _, act := range a.Actions {
			out[act] = true
		}
	}
	return out
}

// missingActions reports which of want are absent from granted, sorted by want's
// order so the message is stable.
func missingActions(granted map[string]bool, want ...string) []string {
	var out []string
	for _, w := range want {
		if !granted[w] {
			out = append(out, w)
		}
	}
	return out
}

// ── cluster + registry I/O (seamed) ──────────────────────────────────────────

var readHarborRobotSecret = func(ns, name string) ([]byte, error) {
	// --ignore-not-found so an ABSENT Secret comes back (empty, nil) and is
	// distinguishable from an unreadable one. The caller retries absence and
	// fails on a real read error; without this both are "exit status 1".
	return execOutput("kubectl", "-n", ns, "get", "secret", name, "--ignore-not-found", "-o", "json")
}

// namespaceExists reports whether the component's namespace is on this cluster at
// all. Seamed alongside the Secret read.
//
// The distinction it buys is the whole reason this gate can be honest. An absent
// llz-cert-automation namespace means the component was never deployed — managed
// App Platform renders a MINIMAL app set and simply does not include it — and a
// gate that failed on that would color.Red every such cluster for a component it was
// never asked to run. An absent Secret INSIDE a present namespace is the real
// finding: ESO is not materializing secret/harbor/robot.
var namespaceExists = func(ns string) (bool, error) {
	out, err := execOutput("kubectl", "get", "namespace", ns, "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// harborHTTP performs a request against the registry. Seamed so the whole
// handshake is testable against an httptest server or canned responses.
var harborHTTP = func(req *http.Request) (*http.Response, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}

// readSecretWithSettle polls for the robot Secret until it appears or the budget
// runs out. Absence is retried; an unreadable cluster fails immediately, because
// "cannot ask" is not the same answer as "not there yet".
func readSecretWithSettle(ns, name string, settle, interval time.Duration) ([]byte, error) {
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		raw, err := readHarborRobotSecret(ns, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::could not read Secret %s/%s (%v)\n", ns, name, err)
			return nil, fmt.Errorf("could not read Secret %s/%s: %w", ns, name, err)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			return raw, nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "::error::the robot credential Secret %s/%s never appeared within %s\n", ns, name, settle)
			return nil, fmt.Errorf("robot credential Secret %s/%s never appeared within %s — ESO has not materialized it "+
				"from secret/harbor/robot; check the harbor-robot-provisioner CronJob has ticked and the ExternalSecret is Ready",
				ns, name, settle)
		}
		fmt.Printf("attempt %d: Secret %s/%s not present yet — retrying in %s\n", attempt, ns, name, interval)
		time.Sleep(interval)
	}
}

// probeHarborRoundTrip runs the full handshake once.
func probeHarborRoundTrip(creds harborRobotCreds, repo string) error {
	base := "https://" + creds.RegistryHost

	// 1. Unauthenticated /v2/ → Bearer challenge.
	req, err := http.NewRequest(http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := harborHTTP(req)
	if err != nil {
		return fmt.Errorf("GET %s/v2/: %w — the registry host is unreachable, which for a host that "+
			"passed the format guard usually means DNS or the ingress, not the credential", base, err)
	}
	challengeHeader := resp.Header.Get("Www-Authenticate")
	status := resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if status == http.StatusOK {
		return fmt.Errorf("GET %s/v2/ returned 200 with no auth challenge — this registry is serving the "+
			"distribution API unauthenticated", base)
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("GET %s/v2/ returned HTTP %d, expected a 401 auth challenge", base, status)
	}
	ch, err := parseBearerChallenge(challengeHeader)
	if err != nil {
		return fmt.Errorf("%s/v2/: %w", base, err)
	}

	// 2. Exchange basic auth for a scoped token.
	tokURL, err := url.Parse(ch.Realm)
	if err != nil {
		return fmt.Errorf("token realm %q is not a URL: %w", ch.Realm, err)
	}
	q := tokURL.Query()
	q.Set("scope", "repository:"+repo+":pull,push")
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	tokURL.RawQuery = q.Encode()

	treq, err := http.NewRequest(http.MethodGet, tokURL.String(), nil)
	if err != nil {
		return err
	}
	treq.SetBasicAuth(creds.Username, creds.Password)
	tresp, err := harborHTTP(treq)
	if err != nil {
		return fmt.Errorf("token request to %s: %w", ch.Realm, err)
	}
	tbody, _ := io.ReadAll(tresp.Body)
	tresp.Body.Close()
	if tresp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("the robot credential was REJECTED by the token service (HTTP 401) — "+
			"robot %q cannot authenticate to %s", creds.Username, creds.RegistryHost)
	}
	if tresp.StatusCode < 200 || tresp.StatusCode >= 300 {
		return fmt.Errorf("token request returned HTTP %d: %s", tresp.StatusCode, truncateForError(tbody))
	}
	token, access, err := parseTokenResponse(tbody)
	if err != nil {
		return err
	}

	// 3. The token must actually GRANT what was asked for. Harbor answers 200
	//    with a valid JWT and an empty access list when the robot lacks the
	//    scope, so the status code proves nothing.
	granted := grantedActions(access, repo)
	if missing := missingActions(granted, "pull", "push"); len(missing) > 0 {
		return fmt.Errorf("the token service issued a token for robot %q that does NOT grant %s on %s "+
			"(granted: %v). A 200 here is not authorization — Harbor returns a valid token with an empty "+
			"access list when the credential lacks the scope, so the robot's project permissions are the thing to check",
			creds.Username, strings.Join(missing, "+"), repo, keysOf(granted))
	}

	// 4. Use it. PULL: tags/list. A repository that does not exist answers
	//    404/NAME_UNKNOWN, which still proves the token was ACCEPTED — the failure
	//    we are hunting is 401, not 404.
	if err := harborAuthedProbe(http.MethodGet, base+"/v2/"+repo+"/tags/list", token,
		map[int]bool{http.StatusOK: true, http.StatusNotFound: true}, "pull"); err != nil {
		return err
	}

	// PUSH: open a blob upload session, then cancel it. This is the cheapest
	// operation that requires genuine push authorization and it leaves nothing.
	loc, err := harborUploadSession(base, repo, token)
	if err != nil {
		return err
	}
	if loc != "" {
		cancelURL := loc
		if strings.HasPrefix(loc, "/") {
			cancelURL = base + loc
		}
		// Best-effort cleanup: the session expires on its own, and failing the gate
		// because a cancel did not land would report a push problem that is not one.
		creq, err := http.NewRequest(http.MethodDelete, cancelURL, nil)
		if err == nil {
			creq.Header.Set("Authorization", "Bearer "+token)
			if cresp, err := harborHTTP(creq); err == nil {
				_, _ = io.Copy(io.Discard, cresp.Body)
				cresp.Body.Close()
			}
		}
	}
	return nil
}

// harborUploadSession opens a blob upload and returns its Location.
func harborUploadSession(base, repo, token string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v2/"+repo+"/blobs/uploads/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := harborHTTP(req)
	if err != nil {
		return "", fmt.Errorf("opening a blob upload session: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("PUSH DENIED: opening a blob upload on %s returned HTTP %d even though the token "+
			"granted push — the registry and the token service disagree about this robot's rights: %s",
			repo, resp.StatusCode, truncateForError(body))
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("opening a blob upload on %s returned HTTP %d, expected 202: %s",
			repo, resp.StatusCode, truncateForError(body))
	}
	return resp.Header.Get("Location"), nil
}

// harborAuthedProbe issues an authenticated request and requires one of okStatus.
func harborAuthedProbe(method, u, token string, okStatus map[int]bool, what string) error {
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := harborHTTP(req)
	if err != nil {
		return fmt.Errorf("%s probe (%s): %w", what, u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s DENIED: %s returned HTTP %d with a token that granted it — %s",
			strings.ToUpper(what), u, resp.StatusCode, truncateForError(body))
	}
	if !okStatus[resp.StatusCode] {
		return fmt.Errorf("%s probe %s returned HTTP %d: %s", what, u, resp.StatusCode, truncateForError(body))
	}
	return nil
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func runCIAssertHarborRoundTrip(secretNS, secretName, registry, repo string, settle, interval time.Duration) error {
	fmt.Println("## Harbor robot round-trip assertion (pull + push authorization)")

	// Is the component here at all? A managed App Platform cluster renders a
	// minimal app set that does not include llz-cert-automation, and on
	// lke638103 the namespace simply did not exist ("namespaces
	// \"llz-cert-automation\" not found"). That is a deployment shape, not a
	// broken credential.
	switch present, err := namespaceExists(secretNS); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "::error::could not tell whether namespace %s exists (%v)\n", secretNS, err)
		return fmt.Errorf("could not determine whether namespace %s exists: %w", secretNS, err)
	case !present:
		fmt.Printf("SKIP: namespace %s does not exist — the llz-cert-automation component is not deployed on this "+
			"cluster (managed App Platform renders a minimal app set), so there is no robot credential to round-trip.\n", secretNS)
		return nil
	}

	// The Secret READ is inside the settle loop, not before it. It is written by
	// ESO from secret/harbor/robot, which the harbor-robot-provisioner CronJob
	// seeds only AFTER Harbor's registry is serving — internal/health/allowlists.go
	// documents it as deferred on a fresh bootstrap and explicitly says it must not
	// pin the convergence gate. Reading once, first, made this gate fail on exactly
	// the window that file says to expect.
	raw, err := readSecretWithSettle(secretNS, secretName, settle, interval)
	if err != nil {
		return err
	}
	creds, err := decodeRobotSecret(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		return err
	}
	if registry != "" {
		creds.RegistryHost = registry
	}
	fmt.Printf("robot %q → registry %s, scope repository:%s:pull,push\n", creds.Username, creds.RegistryHost, repo)

	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		lastErr = probeHarborRoundTrip(creds, repo)
		if lastErr == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: %v — retrying in %s\n", attempt, lastErr, interval)
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Printf("FAIL: %v\n", lastErr)
		fmt.Fprintln(os.Stderr, "::error::the Harbor robot cannot complete a pull+push round trip")
		return fmt.Errorf("harbor robot round trip failed: %w", lastErr)
	}
	fmt.Printf("OK: robot %q authenticated to %s and holds pull+push on %s\n", creds.Username, creds.RegistryHost, repo)
	return nil
}
