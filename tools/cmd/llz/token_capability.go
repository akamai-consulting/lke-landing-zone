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

// capabilityCheck binds a credential to the read-only probe that proves it can
// perform its required operation.
//
// `path` builds the API path from the ambient CI env and returns a skip reason
// instead when the context isn't there — a missing GH_REPO/REGION means we can't
// construct the probe, which is NOT the token's fault and must never fail a run.
// `hint` is the remediation printed on denial; keep it in lockstep with the
// matching `llz ci require-secret --hint` text in the workflows.
type capabilityCheck struct {
	token string
	op    string
	path  func() (path string, skip string)
	hint  string
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
		return capDenied, fmt.Sprintf("auth rejected (HTTP 401) — cannot %s; rotate the token", op)
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
	code, err := ghCapabilityProbe(envOr("GITHUB_API", "https://api.github.com"), token, path)
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
