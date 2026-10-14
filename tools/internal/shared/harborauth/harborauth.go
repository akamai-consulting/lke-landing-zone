// Package harborauth is the Harbor registry-auth client: robot credentials, the
// bearer challenge, and the scope claims a token comes back with.
//
// IT BECAME A PACKAGE ON THE NINTH EXTRACTION, when `objenc` needed it. The
// object-storage encryption gate proves the CA chain by making HARBOR write a
// blob, so it must authenticate as Harbor does — and every one of these helpers
// was private to `llz ci assert-harbor-roundtrip`, which stays. Ten Deps fields
// would have expressed the same dependency while hiding that it is one COHERENT
// client rather than ten unrelated calls.
//
// The three package-level vars (ReadRobotSecret, NamespaceExists, HTTP) are the
// seams tests replace, kept as vars for exactly that reason.
package harborauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const (
	RobotSecretNS   = "llz-cert-automation"
	RobotSecretName = "harbor-docker-config"
)

type RobotCreds struct {
	Username     string
	Password     string
	RegistryHost string
}

// DecodeRobotSecret extracts the robot credential from a Secret's JSON. Pure.
//
// It rejects a registry_host that UsableRegistryHost refuses, using the SAME
// predicate the provisioner uses rather than restating it — a gate that
// re-implemented "looks like a host" would drift from the guard it is backing up,
// and "harbor." passing one but not the other is precisely the bug.
func DecodeRobotSecret(raw []byte) (RobotCreds, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return RobotCreds{}, fmt.Errorf("decoding the robot Secret: %w", err)
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
	var c RobotCreds
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
	if !UsableRegistryHost(c.RegistryHost) {
		return c, fmt.Errorf("registry_host %q is not a usable registry host — this is the truncation class "+
			"(\"harbor.\" is non-empty, so every == \"\" guard passes it) and every push and pull will 401", c.RegistryHost)
	}
	return c, nil
}

// BearerChallenge is the parsed Www-Authenticate header.
type BearerChallenge struct {
	Realm   string
	Service string
}

// ParseBearerChallenge parses a `Bearer realm="…",service="…"` header. Pure.
func ParseBearerChallenge(h string) (BearerChallenge, error) {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return BearerChallenge{}, fmt.Errorf("not a Bearer challenge: %q", h)
	}
	var c BearerChallenge
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

// TokenAccess is the access grant inside a token-service response.
type TokenAccess struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// ParseTokenResponse extracts the token and its granted access. Pure.
func ParseTokenResponse(raw []byte) (string, []TokenAccess, error) {
	var r struct {
		Token       string        `json:"token"`
		AccessToken string        `json:"access_token"`
		Access      []TokenAccess `json:"access"`
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
	// Reading r.Access therefore always yielded an empty list, so GrantedActions
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
func jwtAccessClaims(token string) []TokenAccess {
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
		Access []TokenAccess `json:"access"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims.Access
}

// GrantedActions returns the actions granted for a repository across the access
// list. Pure.
func GrantedActions(access []TokenAccess, repo string) map[string]bool {
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

// MissingActions reports which of want are absent from granted, sorted by want's
// order so the message is stable.
func MissingActions(granted map[string]bool, want ...string) []string {
	var out []string
	for _, w := range want {
		if !granted[w] {
			out = append(out, w)
		}
	}
	return out
}

// ── cluster + registry I/O (seamed) ──────────────────────────────────────────

var ReadRobotSecret = func(ns, name string) ([]byte, error) {
	// --ignore-not-found so an ABSENT Secret comes back (empty, nil) and is
	// distinguishable from an unreadable one. The caller retries absence and
	// fails on a real read error; without this both are "exit status 1".
	return execOutput("kubectl", "-n", ns, "get", "secret", name, "--ignore-not-found", "-o", "json")
}

// NamespaceExists reports whether the component's namespace is on this cluster at
// all. Seamed alongside the Secret read.
//
// The distinction it buys is the whole reason this gate can be honest. An absent
// llz-cert-automation namespace means the component was never deployed — managed
// App Platform renders a MINIMAL app set and simply does not include it — and a
// gate that failed on that would color.Red every such cluster for a component it was
// never asked to run. An absent Secret INSIDE a present namespace is the real
// finding: ESO is not materializing secret/harbor/robot.
//
// IT IS NO LONGER SUFFICIENT ON ITS OWN, and the reason is worth carrying: the
// namespace is now shipped by argoWorkflows on managed clusters, EMPTY, purely so
// argo-helm has somewhere to put the workflow RBAC that controller.
// workflowNamespaces asks for (see that component's cert-automation-namespace.yaml).
// A present-but-empty namespace made this gate stop skipping and start waiting two
// minutes for a Secret nothing would ever create. "The namespace exists" stopped
// meaning "the component is deployed" the moment something else had a reason to
// create it — so the deployment probe moved to RobotExternalSecretExists, which
// tests an object only the chart creates.
var NamespaceExists = func(ns string) (bool, error) {
	out, err := execOutput("kubectl", "get", "namespace", ns, "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// RobotExternalSecretExists reports whether the cert-automation chart's
// ExternalSecret for the robot credential is on this cluster.
//
// THIS IS THE DEPLOYMENT PROBE, not the namespace. The ExternalSecret is created
// only by llz-cert-automation's templates/externalsecrets.yaml, so its presence
// answers "is the component deployed here" without depending on who else might
// have a reason to create the namespace. It also keeps the finding this gate
// exists for intact: ExternalSecret PRESENT and Secret ABSENT still means ESO is
// not materializing secret/harbor/robot, which is a real failure and still fails.
//
// Re-arms by itself: deploy the chart on a managed cluster and the lane starts
// gating again with no code change, the same way the mtls lane resumes when a
// namespace flips to STRICT.
var RobotExternalSecretExists = func(ns, name string) (bool, error) {
	out, err := execOutput("kubectl", "-n", ns, "get", "externalsecret", name, "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// HTTP performs a request against the registry. Seamed so the whole
// handshake is testable against an httptest server or canned responses.
var HTTP = func(req *http.Request) (*http.Response, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}

// readSecretWithSettle polls for the robot Secret until it appears or the budget
// runs out. Absence is retried; an unreadable cluster fails immediately, because
// "cannot ask" is not the same answer as "not there yet".

// ── local glue ───────────────────────────────────────────────────────────────

// UsableRegistryHost rejects the unrendered placeholder and a trailing dot. A copy
// of package main's helper: `llz ci harbor-provisioner` also asks the question, and
// the rule is four characters of logic with nothing to drift.
func UsableRegistryHost(h string) bool {
	s := strings.TrimSpace(h)
	return s != "" && s != "REPLACE_ME" && !strings.HasSuffix(s, ".")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// execOutput is the shell-out seam. A local copy rather than an injected field:
// this package shells out to kubectl in exactly two places, both already seamed as
// vars above, so a Deps struct would add a parameter to every call site to
// configure something the two vars already cover.
var execOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// ── the registry protocol ────────────────────────────────────────────────────

func UploadSession(base, repo, token string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v2/"+repo+"/blobs/uploads/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := HTTP(req)
	if err != nil {
		return "", fmt.Errorf("opening a blob upload session: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("PUSH DENIED: opening a blob upload on %s returned HTTP %d even though the token "+
			"granted push — the registry and the token service disagree about this robot's rights: %s",
			repo, resp.StatusCode, TruncateForError(body))
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("opening a blob upload on %s returned HTTP %d, expected 202: %s",
			repo, resp.StatusCode, TruncateForError(body))
	}
	return resp.Header.Get("Location"), nil
}

// page or a long JSON envelope.
func TruncateForError(b []byte) string {
	const max = 200
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "(empty body)"
	}
	if len(t) > max {
		return t[:max] + "…"
	}
	return t
}
