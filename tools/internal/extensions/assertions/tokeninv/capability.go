package tokeninv

// capability.go — AUTHORIZATION probing, the layer above the VALIDITY
// probe in token_validate.go. Validity answers "does this credential
// authenticate?"; capability answers "is it scoped to do the one job it exists
// for?" Those are different questions, and the gap between them is a real scar:
//
//	21:41  OPENBAO_SECRETS_WRITE_TOKEN  ✓ valid, expires in 77d
//	21:47  llz: gh secret set OPENBAO_SEAL_KEY --env infra-prod: failed to fetch
//	       public key: HTTP 403: Resource not accessible by personal access token
//
// A fine-grained PAT missing "Secrets: write" still authenticates cleanly against
// the API root that tokenprobe.GHPATProbe hits, so it sails through validate-tokens with
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// GHCapabilityProbe GETs one API path with the credential and returns the HTTP
// status (0 == unreachable). Package var so callers are exercisable without
// network access, matching the tokenprobe.GHPATProbe / tokenprobe.LinodeProbe seams.
var GHCapabilityProbe = func(api, token, path string) (int, error) {
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

// GitRefsProbe performs the git smart-HTTP ref-discovery handshake — the FIRST
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
var GitRefsProbe = func(server, token, path string) (int, error) {
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

// LinodeCapabilityProbe GETs one Linode API path with the PAT as a Bearer token
// and returns the HTTP status (0 == unreachable). Same shape and same reason as
// GHCapabilityProbe and tokenprobe.LinodeProbe: a package var so this file is
// exercisable without network access.
//
// NOT tokenprobe.LinodeProbe, which is a different question. That one GETs
// /v4/profile, which EVERY live Linode token can read — it proves the credential
// authenticates and nothing about what it is allowed to do. This one knocks on
// the specific door the pipeline later needs open, which is the entire
// validity-vs-authorization distinction this file exists for.
var LinodeCapabilityProbe = func(api, token, path string) (int, error) {
	url := strings.TrimRight(api, "/") + path
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
	// capRouteRefused — the credential HOLDS the grant this route needs (a second
	// route needing the same grant answered), and this route refused it anyway.
	//
	// A THIRD OUTCOME, BECAUSE THE OTHER TWO WOULD BOTH LIE. capDenied would tell
	// the operator to re-scope a PAT that is correctly scoped, and block a build on
	// it; capUnknown would file a measured, reproducible refusal under "could not
	// verify" and lose it. What it actually means is that a check downstream of this
	// route is INERT — it will warn and pass forever — and that is a finding worth
	// naming rather than either failing on or forgetting. Non-blocking, and loud.
	capRouteRefused
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
	capREST   capTransport = iota // GET {GITHUB_API}{path}, PAT in an Authorization header
	capGit                        // GET {GITHUB_SERVER_URL}{path}, git smart-HTTP + Basic auth
	capLinode                     // GET {LINODE_API}{path}, PAT as a Bearer token
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

	// secondOpinion names a SECOND read-only route needing the SAME grant as
	// `path`, asked only when the first one refuses. Its job is to tell a missing
	// GRANT from a refusing ROUTE, which a single probe cannot do: both answer 401.
	//
	// It varies exactly one thing — the route — and holds the transport, the
	// credential and the host fixed, because anything else it varied would be a
	// second explanation for the difference and the verdict would stop meaning
	// anything.
	//
	// Only register one where the two routes genuinely share a grant. A second
	// route needing a DIFFERENT permission would acquit an under-scoped token, which
	// is the failure this whole file exists to prevent, arrived at from the other
	// side.
	//
	// WHAT IT ISOLATES IS THE ROUTE, AND ONLY THE ROUTE. Two routes that share a
	// grant also share everything above it — the API version, the account, the
	// platform — so a cause at that level refuses both and reads here as a missing
	// grant. That is why the denial message names the account-level candidates
	// instead of asserting the scope one. It is a real limit and it is bounded: every
	// cause it cannot separate stops the build this preflight runs ahead of anyway,
	// so the VERDICT is right even where the explanation is uncertain.
	secondOpinion func() (path string, op string)
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
	{
		token: "LINODE_API_TOKEN",
		op:    "read this account's LKE-Enterprise version catalog",
		// THE EXACT ROUTE `llz ci assert-k8s-version` READS, taken from
		// linode.LKEVersionsPath rather than spelled here — see that function for why
		// a probe with its own copy of the route is worse than no probe at all.
		//
		// WHY THIS ONE IS NOT LIKE ITS NEIGHBOURS. The other two checks exist because
		// a later WRITE would 403; nothing downstream of them changes if the probe is
		// skipped. This one exists because a later READ is allowed to fail SOFTLY:
		// `llz ci assert-k8s-version` warns and passes when this route refuses it, on
		// the deliberate rule (#426) that a build must not be blocked on a question
		// nobody could ask. That rule is right and it has a cost — a token that ALWAYS
		// refuses leaves the gate permanently inert while every run stays green, which
		// is issue #449. Nothing measured whether the question was ever answerable.
		// This does, once per pipeline, at the credential rather than at the gate.
		transport: capLinode,
		path: func() (string, string) {
			// No ambient context to assemble and therefore no skip arm: the route is a
			// constant, so this check either runs or the token is unset. The two GitHub
			// checks skip on a missing GH_REPO/REGION; there is no equivalent here, and
			// inventing one would be a way for this to go quiet.
			return linode.LKEVersionsPath(linode.LKETierEnterprise), ""
		},
		// Asked ONLY if the versions route refuses. Both routes are `lke:read_only`,
		// so this is the discriminator: refused at both ⇒ the PAT lacks the grant, and
		// the cluster apply would have failed on it anyway (blocking, and strictly
		// earlier than the ~15 minutes that costs today). Accepted here and refused
		// there ⇒ the grant is present, and the refusal belongs to the route.
		secondOpinion: func() (string, string) {
			return linode.LKEClustersPath, "list this account's LKE clusters (the same lke:read_only grant)"
		},
		hint: "the Linode PAT needs Kubernetes (LKE): Read Only or better — it is refused at BOTH the " +
			"LKE-Enterprise version catalog and the plain cluster list, so this is not one fussy route. " +
			"If the PAT visibly carries that grant, the remaining candidates are account-level rather " +
			"than token-level (LKE not enabled on the account, an API restriction) — and all of them " +
			"stop the same build: unfixed, `terraform apply` fails ~15 minutes in when it tries to " +
			"create the LKE-E cluster, and `llz ci assert-k8s-version` cannot check the pin that would " +
			"have caught a bad one before the apply started",
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
func classifyCapabilityStatus(code int, op string, t capTransport) (capabilityStatus, string) {
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
		// particular door refuses it.
		return capDenied, fmt.Sprintf("auth rejected (HTTP 401) — the token is live but not accepted to %s "+
			"(%s); %s, don't rotate it", op, refusalCauses(t), refusalRemedy(t))
	case code == 404:
		return capUnknown, fmt.Sprintf("HTTP 404 probing %q — either the target does not exist yet or this token cannot see it; could not verify", op)
	default:
		return capUnknown, fmt.Sprintf("unexpected HTTP %d — could not verify authorization", code)
	}
}

// refusalCauses names what a 401 at THIS door plausibly means, because the causes
// do not overlap and the remedies are different buildings. SAML SSO authorization
// is a GitHub concept and offering it as a cause for a Linode PAT sends the
// operator to a setting that does not exist; conversely a Linode PAT's grants are
// per-token checkboxes with no org-authorization step at all.
func refusalCauses(t capTransport) string {
	if t == capLinode {
		return "the PAT is missing the grant this route needs"
	}
	return "missing permission, or SAML SSO not authorized"
}

// refusalRemedy is the ACTION half, and it is transport-aware for the same reason
// the cause is — with one scar attached. Making only the cause vary dropped
// "SSO-authorize it" from the GitHub 401, which is the remedy for the second of
// the two causes that line names. A correctly-scoped APL_VALUES_REPO_TOKEN that is
// simply not SSO-authorized then blocked the run under advice that cannot fix it,
// and the test guarding this only checked that the CAUSE still said SSO.
func refusalRemedy(t capTransport) string {
	if t == capLinode {
		return "re-scope it"
	}
	return "re-scope or SSO-authorize it"
}

// probeOneRoute performs a single read-only GET on the check's transport and
// classifies the answer. Split out of probeCapability so the second opinion asks
// the SAME question of a different route rather than a similar-looking one.
func probeOneRoute(t capTransport, token, path, op string) (capabilityStatus, string, int) {
	var code int
	var err error
	switch t {
	case capGit:
		code, err = GitRefsProbe(envOr("GITHUB_SERVER_URL", "https://github.com"), token, path)
	case capLinode:
		code, err = LinodeCapabilityProbe(envOr("LINODE_API", linode.APIBase), token, path)
	default:
		code, err = GHCapabilityProbe(envOr("GITHUB_API", "https://api.github.com"), token, path)
	}
	if err != nil {
		code = 0
	}
	st, detail := classifyCapabilityStatus(code, op, t)
	// The raw code travels alongside the rendered detail. A caller that has to
	// OVERRIDE the verdict must state the bare fact rather than quote a sentence
	// written for the verdict it is overriding — the first cut of the route-refused
	// arm pasted the denial detail in and told the operator to "re-scope" a
	// correctly-scoped PAT in the same breath as saying its scope was fine.
	return st, detail, code
}

// probeCapability runs one credential's authorization check against a token
// value already known to be present.
//
// A REFUSAL IS NOT YET A VERDICT WHEN A SECOND OPINION IS REGISTERED. One 401
// cannot tell "this credential lacks the grant" from "this route refuses a
// credential that has it", and those want opposite handling: the first is a
// build-blocking credential fault, the second is a downstream check going inert.
// Asking a second route that needs the SAME grant separates them, and it is asked
// ONLY on a refusal — a good run still pays exactly one request.
func probeCapability(c capabilityCheck, token string) capabilityResult {
	path, skip := c.path()
	if skip != "" {
		return capabilityResult{c.token, capSkipped, skip}
	}
	s, d, code := probeOneRoute(c.transport, token, path, c.op)
	if s != capDenied || c.secondOpinion == nil {
		return capabilityResult{c.token, s, d}
	}

	p2, op2 := c.secondOpinion()
	s2, d2, _ := probeOneRoute(c.transport, token, p2, op2)
	switch s2 {
	case capOK:
		// THE GRANT IS THERE AND THE ROUTE REFUSED ANYWAY — measured, not inferred.
		// This is the sentence issue #449 wanted something to be able to say: the
		// question `llz ci assert-k8s-version` asks cannot be asked in this pipeline,
		// so that gate is inert here and its green step means nothing. Naming the
		// consequence is the point; a verdict about a credential that the reader has
		// to connect to a gate three steps later is the silence all over again.
		return capabilityResult{c.token, capRouteRefused, fmt.Sprintf(
			"the token IS authorized to %s, and is refused (HTTP %d) when it tries to %s — same grant, "+
				"different door, so THIS IS THE ROUTE AND NOT THE TOKEN. Nothing to re-scope. "+
				"`llz ci assert-k8s-version` reads that route and warns-and-PASSES when it is refused, so the "+
				"k8sVersion preflight is INERT in this pipeline: its green step is not evidence, and a pin this "+
				"account cannot build will reach `terraform apply` unchecked", op2, code, c.op)}
	case capDenied:
		// REFUSED AT BOTH, WHICH RULES OUT THE ROUTE AND NOT MUCH ELSE. The overwhelmingly
		// likely cause is the missing grant, and that is what the hint leads with — but
		// two routes under the same API version can also share an explanation that has
		// nothing to do with scope (a v4beta-wide restriction, a suspended account). The
		// verdict is the same either way, because every one of those breaks the cluster
		// apply this preflight runs ahead of; the MESSAGE must not assert the one it
		// cannot distinguish, or an operator spends the afternoon re-scoping a PAT that
		// was never the problem.
		return capabilityResult{c.token, capDenied, fmt.Sprintf(
			"%s; it is also refused to %s, which needs the same grant — so this is not one fussy route. "+
				"Almost always the missing grant; if the PAT visibly has it, look for an account-level "+
				"restriction on the Linode API rather than at the token", d, op2)}
	default:
		// The corroborating read could not be made, so the refusal stands unexplained.
		// Blocking here would fail a build on a blip at a route nothing was asking
		// about; that is the trade this file already makes for a 404.
		//
		// THE BARE FACT, NOT THE FIRST PROBE'S RENDERED DETAIL. That detail ends
		// "re-scope it, don't rotate it" — a remedy this arm has just declared it
		// cannot justify. Pasting it in is the same contradiction the capOK arm above
		// was fixed for, and probeOneRoute returns the code precisely so neither arm
		// has to quote a sentence written for the verdict it is overriding.
		return capabilityResult{c.token, capUnknown, fmt.Sprintf(
			"refused (HTTP %d) when it tries to %s, and the second opinion could not be taken (%s) — "+
				"so whether this is the token's scope or the route itself is UNRESOLVED. Nothing to act on "+
				"yet; re-run, and if it reproduces the scope line below will say which", code, c.op, d2)}
	}
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
		return color.Green("✓ " + cr.detail)
	case capDenied:
		return color.Red("✗ DENIED — " + cr.detail)
	case capUnknown:
		return color.Yellow("⚠ " + cr.detail)
	case capRouteRefused:
		return color.Yellow("⚠ INERT — " + cr.detail)
	default: // capSkipped
		return color.Dim("– " + orDefault(cr.detail, "not probed"))
	}
}

// orDefault is a four-line local copy rather than an export from
// internal/shared/tokenprobe. It stayed unexported there because exporting it
// would widen that package's API for a string fallback, and this is cheaper.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
