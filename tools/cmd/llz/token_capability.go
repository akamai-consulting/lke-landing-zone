package main

// token_capability.go — AUTHORIZATION probing, the layer above the VALIDITY
// probe in token_validate.go. Validity answers "does this credential
// authenticate?"; capability answers "is it scoped to do the one job it exists
// for?" Those are different questions, and the gap between them is a real scar:
//
//	21:41  OPENBAO_SECRETS_WRITE_TOKEN  ✓ valid, expires in 77d
//	21:47  llz: gh secret set OPENBAO_SEAL_KEY --env infra-prod: failed to fetch
//	       public key: HTTP 403: Resource not accessible by personal access token
//
// A fine-grained PAT missing "Secrets: write" still authenticates cleanly against
// the API root that ghPATProbe hits, so it sails through validate-tokens with
// months of life left and then 403s six minutes later — AFTER the cluster, apl-
// core, Kyverno and the Argo bridge are already up. That failure lands past the
// `foundation-ready` phase mark, leaving a half-configured deployment with no
// seal key (apl-operator crash-looping on a missing apl-sops-secrets, Harbor
// without its secret-key) that a human has to unwind.
//
// The fix is to probe the ACTUAL operation, read-only. Every check here is a GET
// against the exact endpoint the later write uses, so a 403 here is precisely the
// 403 that would have failed the run — no scope inference, no side effects, no
// mutation. The `require-secret` hints already DOCUMENT the needed scopes; this
// verifies them.
//
// CI-only by construction, like ci_validate_tokens.go: GitHub exposes secret
// values only inside a job, never to `llz doctor` on a laptop.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ghCapabilityProbe GETs one API path with the credential and returns the HTTP
// status (0 == unreachable). Package var so callers are exercisable without
// network access, matching the ghPATProbe / linodeProbe seams.
var ghCapabilityProbe = func(api, token, path string) (int, error) {
	url := strings.TrimRight(api, "/") + path
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err // unreachable — code 0
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// gitRefsProbe performs the git smart-HTTP ref-discovery handshake — the FIRST
// request any `git clone`/`git fetch`/`ls-remote` makes, and the one Argo CD's
// repo-server makes when it computes an Application's target state. It is a
// plain GET, read-only, and transfers no objects.
//
// This exists because the REST API and the git endpoint are different doors with
// different locks. A PAT can pass every api.github.com probe and still be refused
// at github.com/<repo>.git — SAML SSO not authorized for the org, an IP
// allowlist, or a fine-grained PAT without Contents. The failure text this is
// built to catch is Argo's verbatim:
//
//	gitops-global — ComparisonError: failed to generate manifest: rpc error:
//	  code = Unknown desc = failed to list refs: authentication required: Unauthorized
//
// "failed to list refs" IS this request failing. Probing it in preflight asks
// the server the identical question ~40 minutes earlier.
//
// Username is the `x-access-token` convention authedGitURL already uses: GitHub
// ignores it for a PAT, but the Basic header must carry something.
var gitRefsProbe = func(server, token, path string) (int, error) {
	url := strings.TrimRight(server, "/") + path
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("x-access-token", token)
	// Git clients send this; without it GitHub answers the "dumb" protocol and
	// the status stops reflecting what a real clone would see.
	req.Header.Set("User-Agent", "git/2.43.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err // unreachable — code 0
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

type capabilityStatus int

const (
	capSkipped capabilityStatus = iota // context missing (no GH_REPO/REGION) → not probed
	capOK                              // authorized for the operation
	capDenied                          // authenticates but NOT authorized — the scar case
	capUnknown                         // unreachable or ambiguous → warn, never block
)

// capabilityResult is one credential's authorization verdict.
type capabilityResult struct {
	name   string
	status capabilityStatus
	detail string
}

// capTransport selects WHICH door the probe knocks on. The two are not
// interchangeable: a credential authorized at one can be refused at the other,
// which is the whole reason capGit exists alongside capREST.
type capTransport int

const (
	capREST capTransport = iota // GET {GITHUB_API}{path}, PAT in an Authorization header
	capGit                      // GET {GITHUB_SERVER_URL}{path}, git smart-HTTP + Basic auth
)

// capabilityCheck binds a credential to the read-only probe that proves it can
// perform its required operation.
//
// `path` builds the request path from the ambient CI env and returns a skip
// reason instead when the context isn't there — a missing GH_REPO/REGION means we
// can't construct the probe, which is NOT the token's fault and must never fail a
// run. `hint` is the remediation printed on denial; keep it in lockstep with the
// matching `llz ci require-secret --hint` text in the workflows.
type capabilityCheck struct {
	token     string
	op        string
	transport capTransport
	path      func() (path string, skip string)
	hint      string
}

// capabilityChecks lists every credential whose SCOPE (not just validity) is
// verified. Only add an entry whose probe is the read-only twin of the exact
// call the pipeline later makes — an inferred check (e.g. reading
// `permissions.push` off the repo object to guess at Contents: write) can be
// wrong in both directions, and a false denial blocks a run for no reason.
var capabilityChecks = []capabilityCheck{
	{
		token: "OPENBAO_SECRETS_WRITE_TOKEN",
		op:    "write infra-<region> environment secrets",
		// The exact endpoint `gh secret set --env infra-<region>` fetches before
		// it can encrypt a value: no public key, no secret write. Read-only.
		path: func() (string, string) {
			repo, region := os.Getenv("GH_REPO"), os.Getenv("REGION")
			if repo == "" || region == "" {
				return "", "GH_REPO/REGION unset — cannot build the environment-secret probe"
			}
			return fmt.Sprintf("/repos/%s/environments/infra-%s/secrets/public-key", repo, region), ""
		},
		// Keep this wording in lockstep with wizard.go's catalog note and the
		// require-secret hint in llz-bootstrap-openbao.yml — three copies of the
		// same remediation that must not drift. It names ENVIRONMENTS explicitly
		// because the intuitive answer ("Secrets: write") is the wrong one and
		// sends the operator to a toggle that changes nothing.
		hint: "fine-grained PAT needs Environments: write on this repo — NOT \"Secrets: write\", which governs only " +
			"repo-level secrets and leaves environment-secret writes 403ing (a classic repo+workflow PAT also works). " +
			"The PAT owner must additionally be an Environment admin on infra-<region>. Without this, " +
			"`llz ci bao-seed-seal-key` cannot persist OPENBAO_SEAL_KEY and the deployment is left unsealable",
	},
	{
		token:     "APL_VALUES_REPO_TOKEN",
		op:        "fetch the values repo over git (what Argo CD's repo-server does)",
		transport: capGit,
		// Ref discovery: the first request of every clone/fetch/ls-remote, and the
		// one whose failure Argo reports as "failed to list refs". Read-only — it
		// negotiates refs and stops, transferring no objects.
		//
		// This is a NECESSARY condition for the token's job, not a sufficient one:
		// apl-operator PUSHES its rendered values tree, which needs Contents:WRITE,
		// and no read-only probe can prove write without writing. A token that
		// fails here certainly cannot push; one that passes might still lack write.
		// That asymmetry is deliberate — the alternative is inferring write from
		// `permissions.push` on the repo object, which is wrong in both directions.
		path: func() (string, string) {
			repo := os.Getenv("GH_REPO")
			if repo == "" {
				return "", "GH_REPO unset — cannot build the values-repo fetch probe"
			}
			return "/" + repo + ".git/info/refs?service=git-upload-pack", ""
		},
		hint: "fine-grained PAT needs Contents: write on this repo, and must be authorized for the org's " +
			"SAML SSO if one is enforced — the git endpoint (github.com) rejects independently of the REST API " +
			"(api.github.com), so a token that passes every other preflight can still be refused here. This " +
			"credential becomes apl-core's otomi.git.password, which apl-operator pushes the values tree with " +
			"and which reaches Argo CD as its values-repo credential; without a working fetch every gitops-* " +
			"Application ComparisonErrors with \"failed to list refs: authentication required\" and the whole " +
			"external-secrets/cert chain behind it never installs",
	},
}

// classifyCapabilityStatus maps a probe status to an authorization verdict.
//
// 403 is the unambiguous scar: the credential authenticated (it got past auth)
// and was refused the resource — under-scoped, and it WILL fail the same way
// later, so it blocks. 404 is deliberately NOT blocking: GitHub returns it both
// for "the environment doesn't exist yet" and for "this PAT can't see it", and
// those are indistinguishable from here — failing a run on that ambiguity would
// trade a late true positive for an early false one. Warn with both causes and
// let the run proceed to the real call.
func classifyCapabilityStatus(code int, op string) (capabilityStatus, string) {
	switch {
	case code == 0:
		return capUnknown, "endpoint unreachable — could not verify authorization (not failing on connectivity)"
	case code/100 == 2:
		return capOK, "authorized to " + op
	case code == 403:
		return capDenied, fmt.Sprintf("authenticates, but is NOT authorized to %s (HTTP 403) — the token is under-scoped, not expired", op)
	case code == 401:
		// NOT "rotate it": capability is only asked of a credential whose validity
		// probe already passed, so the token is live. A 401 here means this
		// particular door refuses it — the git endpoint's answer for a PAT lacking
		// the repo's Contents permission or unauthorized for an SSO-enforced org.
		return capDenied, fmt.Sprintf("auth rejected (HTTP 401) — the token is live but not accepted to %s "+
			"(missing permission, or SAML SSO not authorized); re-scope or SSO-authorize it, don't rotate it", op)
	case code == 404:
		return capUnknown, fmt.Sprintf("HTTP 404 probing %q — either the target does not exist yet or this token cannot see it; could not verify", op)
	default:
		return capUnknown, fmt.Sprintf("unexpected HTTP %d — could not verify authorization", code)
	}
}

// probeCapability runs one credential's authorization check against a token
// value already known to be present.
func probeCapability(c capabilityCheck, token string) capabilityResult {
	path, skip := c.path()
	if skip != "" {
		return capabilityResult{c.token, capSkipped, skip}
	}
	var code int
	var err error
	switch c.transport {
	case capGit:
		code, err = gitRefsProbe(envOr("GITHUB_SERVER_URL", "https://github.com"), token, path)
	default:
		code, err = ghCapabilityProbe(envOr("GITHUB_API", "https://api.github.com"), token, path)
	}
	if err != nil {
		code = 0
	}
	s, d := classifyCapabilityStatus(code, c.op)
	return capabilityResult{c.token, s, d}
}

// checkCapability runs the capability check registered for a credential, if any.
// The bool is false when the credential has no scope requirement to verify —
// most don't, and the caller prints nothing for those.
func checkCapability(name, token string) (capabilityResult, bool) {
	for _, c := range capabilityChecks {
		if c.token == name {
			return probeCapability(c, token), true
		}
	}
	return capabilityResult{}, false
}

// capabilityHint returns the remediation text for a denied credential.
func capabilityHint(name string) string {
	for _, c := range capabilityChecks {
		if c.token == name {
			return c.hint
		}
	}
	return ""
}

// capabilityCell renders one verdict for the report table.
func capabilityCell(cr capabilityResult) string {
	switch cr.status {
	case capOK:
		return green("✓ " + cr.detail)
	case capDenied:
		return red("✗ DENIED — " + cr.detail)
	case capUnknown:
		return yellow("⚠ " + cr.detail)
	default: // capSkipped
		return dim("– " + orDefault(cr.detail, "not probed"))
	}
}
