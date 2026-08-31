package tokenprobe

// capability.go — AUTHORIZATION probing, the layer above the VALIDITY probe in
// probe.go. Validity answers "does this credential authenticate?"; capability
// answers "is it scoped to do the one job it exists for?" Those are different
// questions, and the gap between them is a real scar:
//
//	21:41  OPENBAO_SECRETS_WRITE_TOKEN  ✓ valid, expires in 77d
//	21:47  llz: gh secret set OPENBAO_SEAL_KEY --env infra-prod: failed to fetch
//	       public key: HTTP 403: Resource not accessible by personal access token
//
// A fine-grained PAT missing "Secrets: write" still authenticates cleanly against
// the API root that GHPATProbe hits, so it sails through validate-tokens with
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
// IT WAS CI-ONLY, AND THAT WAS HALF A GATE. The file used to say so in its own
// header — "GitHub exposes secret values only inside a job, never to `llz doctor`
// on a laptop" — and the sentence is true of secrets that live ONLY on GitHub. It
// is not true of the ones the wizard gathered, which sit in .llz/secrets.env on
// the operator's disk and which the VALIDITY probe next door has always read from
// there. So authorization was measured in the one place the operator is not
// looking (a CI log, mid-run) and left unmeasured in the one place they are
// (`llz tokens` / `llz doctor`, before the run), for no reason but where the code
// happened to live. It lives here now, ambient CI env is a CapContext the caller
// supplies, and both callers ask the same question of the same catalog.
//
// WHY IT IS SUBSTRATE. The verdict has to reach envreq.ReportReadiness to become
// a table column, and internal/shared must never import internal/extensions
// (extension.layering_test). Same move, same reason, as the validity model that
// preceded it here.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
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

// CapContext is the deployment a capability probe is asked ABOUT: which repo
// holds the secrets and environments, and which region names the infra-<region>
// environment the build writes back to.
//
// IT IS A PARAMETER BECAUSE THE ANSWER DIFFERS BY CALLER, not because parameters
// are tidier. In CI the two values are ambient (GH_REPO/REGION, set by the
// workflow); at `llz doctor` they are arguments the operator typed and the
// resolver worked out, and there is no GH_REPO in that process at all. Reading
// env here — which is what this did — silently degraded every local probe to
// "context missing → skipped", i.e. exactly the quiet pass the file exists to
// abolish, and it would have done so while printing a column that looked probed.
type CapContext struct {
	Repo   string // owner/name
	Region string // deployment name; infra-<Region> is the environment

	// ComponentOff names the LLZ components this deployment does NOT emit
	// (clusterspec.DisabledComponents). A check whose consumer lives in one of
	// them is skipped rather than denied.
	//
	// AN OFF-SET, AND ABSENT MEANS PRESENT. A caller with no readable spec passes
	// nil and every check still runs, which is the conservative direction: the
	// grant is asked for on a deployment that may not need it, rather than
	// silently dropped on one that does. Inverting this to an on-set would make an
	// unreadable spec switch every conditional check off — the silence these
	// checks exist to end.
	ComponentOff map[string]bool
}

// EnvCapContext reads the CI-ambient context. The workflows export both, so this
// is what `llz ci validate-tokens` passes; a local caller builds the struct from
// what it already resolved instead. ComponentOff is left nil — the caller fills
// it from the spec, because reading one is not this package's job.
func EnvCapContext() CapContext {
	return CapContext{Repo: os.Getenv("GH_REPO"), Region: os.Getenv("REGION")}
}

type CapabilityStatus int

const (
	CapSkipped CapabilityStatus = iota // context missing (no repo/region) → not probed
	CapOK                              // authorized for the operation
	CapDenied                          // authenticates but NOT authorized — the scar case
	CapUnknown                         // unreachable or ambiguous → warn, never block
	// CapRouteRefused — the credential HOLDS the grant this route needs (a second
	// route needing the same grant answered), and this route refused it anyway.
	//
	// A THIRD OUTCOME, BECAUSE THE OTHER TWO WOULD BOTH LIE. CapDenied would tell
	// the operator to re-scope a PAT that is correctly scoped, and block a build on
	// it; CapUnknown would file a measured, reproducible refusal under "could not
	// verify" and lose it. What it actually means is that a check downstream of this
	// route is INERT — it will warn and pass forever — and that is a finding worth
	// naming rather than either failing on or forgetting. Non-blocking, and loud.
	CapRouteRefused
	// CapNotApplicable — this deployment does not deploy the grant's consumer, so
	// there is no question to put.
	//
	// NOT CapSkipped, WHICH IS WHAT IT WAS. Skipped means "we could not ask", and
	// everything downstream is written for that meaning: the column renders
	// "· partial" in yellow, the notes loop prints "scope NOT verified", and the
	// operator of a correctly configured harbor-less instance got a standing
	// warning saying, in one breath, "scope NOT verified — not applicable". Nothing
	// failed to be verified; there was nothing to verify. A permanent yellow on a
	// configuration that will never change is how a column stops being read, which
	// would cost exactly the attention this whole file exists to buy.
	CapNotApplicable
)

// CapabilityResult is one credential's authorization verdict for ONE registered
// check. A credential may have several (OPENBAO_SECRETS_WRITE_TOKEN needs two
// different GitHub Secrets permissions for two different consumers), so callers
// receive a slice and must render every element — collapsing them to the first
// is how the repo-level check would have gone missing again.
type CapabilityResult struct {
	Name   string
	Op     string // the operation probed, for a caller that groups by check
	Status CapabilityStatus
	Detail string
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
// `path` builds the request path from the CapContext and returns a skip reason
// instead when the context isn't there — a missing repo/region means we can't
// construct the probe, which is NOT the token's fault and must never fail a run.
//
// `hint` is the remediation printed on denial. Keep it in lockstep with the other
// four copies of the same advice — the environment-secret check below lists them,
// and records what happened when one was missed.
type capabilityCheck struct {
	token     string
	op        string
	transport capTransport
	path      func(CapContext) (path string, skip string)
	hint      string

	// component names the LLZ component whose content is this grant's ONLY
	// consumer. When the deployment does not emit that component the grant has
	// nothing to be needed by, and the check is skipped instead of denied.
	//
	// EVERY OTHER CHECK IN THIS CATALOG IS UNCONDITIONAL — seal-key writes, the
	// values-repo fetch and LKE cluster-create have consumers no instance can turn
	// off — so this field was not needed until the repo-level Secrets check, whose
	// consumer is the harbor-robot-provisioner CronJob inside `harbor`. Without it
	// an instance that opts out of Harbor is hard-blocked at `llz doctor`,
	// `llz tokens` and `llz ci validate-tokens`, told to re-scope a PAT for a
	// permission nothing in its cluster will ever use — the "a false denial blocks
	// a run for no reason" this catalog's own docstring names as the thing not to
	// do, arrived at from the other side.
	component string

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
		path: func(cc CapContext) (string, string) {
			if cc.Repo == "" || cc.Region == "" {
				return "", "repo/region unknown — cannot build the environment-secret probe"
			}
			return fmt.Sprintf("/repos/%s/environments/infra-%s/secrets/public-key", cc.Repo, cc.Region), ""
		},
		// SCOPED TO THIS ENDPOINT, and it has to say so. This read "needs
		// Environments: write — NOT \"Secrets: write\"", which was written when
		// Environments was the only grant this PAT needed and is now the single
		// most misleading sentence the tool can print: the repo-level Secrets check
		// below DEMANDS the permission this line tells the operator to withhold, so
		// an Environments denial would hand them advice that fails the other check.
		// "Not Secrets" is true of THIS route and false of this credential.
		//
		// SEVEN CARRIERS OF THIS REMEDIATION, and they must not drift:
		//
		//   1. ghFineGrainedSecretsWriteURL — the pre-filled creation link, and the
		//      only one that decides what the PAT is ACTUALLY scoped for
		//   2. catalog()'s Note                          (wizard.go)
		//   3. secretsWritePATLabel                      (tokens.go, on screen)
		//   4. this hint                                 (Environments)
		//   5. the repo-secrets hint below               (Secrets)
		//   6. llz-bootstrap-openbao.yml's require-secret --hint
		//   7. llz-terraform.yml's require-secret --hint
		//
		// Every one is gated: 1-3 and 6-7 by onboard's pat_guidance_drift_test.go
		// (which reads the two workflows off disk), 4-5 by
		// TestBothHintsOnThisPATNameBothGrants here.
		//
		// AN EARLIER VERSION OF THIS COMMENT SAID FIVE and listed 2-7, omitting the
		// creation URL — the carrier that mints the token, and therefore the one whose
		// drift produces an under-scoped PAT rather than merely bad advice about one.
		// A round of fixes corrected four carriers and missed this hint; the next
		// corrected this and missed the label. An enumeration is only worth writing if
		// it is complete, and this one was not.
		hint: "fine-grained PAT needs Environments: write on this repo — the permission that governs infra-<region> " +
			"ENVIRONMENT secrets. \"Secrets: write\" does NOT cover these (it governs repo-level secrets only), so " +
			"granting that instead leaves environment-secret writes 403ing — but do not read this as a reason to " +
			"withhold it: this same PAT needs Secrets: write as well, for the repo-level HARBOR_* secrets the " +
			"in-cluster harbor-robot-provisioner publishes, and that grant is checked separately. Grant BOTH (a " +
			"classic repo+workflow PAT carries both). The PAT owner must additionally be an Environment admin on " +
			"infra-<region>. Without Environments, `llz ci bao-seed-seal-key` cannot persist OPENBAO_SEAL_KEY and " +
			"the deployment is left unsealable",
	},
	{
		token: "OPENBAO_SECRETS_WRITE_TOKEN",
		op:    "read/write REPO-level Actions secrets",
		// THE SECOND GRANT ON THE SAME PAT, and the one nothing checked.
		//
		// The check above verifies Environments: write, which is what the BUILD
		// needs. This one verifies repo-level Secrets, which is what the CLUSTER
		// needs: `llz ci bao-seed-all` seeds this same PAT to
		// secret/infra/github-dispatch-token, ESO syncs it into
		// harbor/harbor-provisioner-gh-token, and the harbor-robot-provisioner
		// CronJob calls forge's RepoSecretExists/SetRepoSecret with it every five
		// minutes to publish HARBOR_ROBOT_NAME/HARBOR_PASSWORD/HARBOR_PULL_* —
		// the repo-level secrets a standby's `llz ci seed-standby-harbor-robots`
		// then seeds its own OpenBao from.
		//
		// WITHOUT IT THE FAILURE IS SILENT FOR ~35 MINUTES AND THEN TERMINAL:
		//
		//	llz: check repo secret HARBOR_ROBOT_NAME: HTTP 403:
		//	     {"message":"Resource not accessible by personal access token"}
		//
		// every provisioner tick, three failed Jobs standing in `harbor`, and
		// converge hard-failing the bootstrap on them twice in a row — after the
		// cluster, apl-core and Argo are up. The PAT is live, in date, and passes
		// the environment-secret check next door, because the two are DIFFERENT
		// fine-grained permissions and the guidance we ship says to grant only the
		// other one ("Environments: write, NOT Secrets" — quickstart §token). That
		// sentence is right about environment secrets and wrong as a whole: this
		// deployment needs both, and the one place it was ever going to be noticed
		// was a 403 in a CronJob log nobody reads.
		//
		// Read-only, and NOT a probe of one HARBOR_* secret: those legitimately do
		// not exist before the first provisioner run, and a 404 would be
		// indistinguishable from a refusal. The collection's public key always
		// exists and needs the same permission.
		//
		// NECESSARY, NOT SUFFICIENT — the same asymmetry APL_VALUES_REPO_TOKEN's
		// git check documents. A read-only GET proves the PAT is not shut out of
		// repo secrets; only a write proves write, and this preflight does not
		// write.
		// The provisioner ships inside `harbor` (its ManifestResources name
		// harbor/harbor-robot-provisioner), and on the Managed App Platform that
		// component additionally emits only when the operator lists `harbor` in
		// bootstrap.managedApps. Both paths are the same question, and
		// clusterspec.ComponentEmits is the one function that answers it.
		component: "harbor",
		path: func(cc CapContext) (string, string) {
			if cc.Repo == "" {
				return "", "repo unknown — cannot build the repo-secret probe"
			}
			return forge.RepoSecretsPath(cc.Repo) + "/public-key", ""
		},
		hint: "fine-grained PAT needs Secrets: read and write on this repo (required only while the `harbor` " +
			"component is enabled — its CronJob is the sole consumer) — a SEPARATE permission from the " +
			"Environments: write the same token needs for infra-<region> writes, so granting one does not " +
			"grant the other (a classic repo+workflow PAT carries both). Without it the in-cluster " +
			"harbor-robot-provisioner CronJob 403s on every tick, never publishes HARBOR_ROBOT_NAME / " +
			"HARBOR_PASSWORD / HARBOR_PULL_*, leaves failed Jobs in the harbor namespace that hard-fail " +
			"`llz ci converge`, and leaves a standby peer with nothing to seed its Harbor robots from",
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
		path: func(cc CapContext) (string, string) {
			if cc.Repo == "" {
				return "", "repo unknown — cannot build the values-repo fetch probe"
			}
			return "/" + cc.Repo + ".git/info/refs?service=git-upload-pack", ""
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
		path: func(CapContext) (string, string) {
			// No context to assemble and therefore no skip arm: the route is a
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
func classifyCapabilityStatus(code int, op string, t capTransport) (CapabilityStatus, string) {
	switch {
	case code == 0:
		return CapUnknown, "endpoint unreachable — could not verify authorization (not failing on connectivity)"
	case code/100 == 2:
		return CapOK, "authorized to " + op
	case code == 403:
		return CapDenied, fmt.Sprintf("authenticates, but is NOT authorized to %s (HTTP 403) — the token is under-scoped, not expired", op)
	case code == 401:
		// NOT "rotate it": capability is only asked of a credential whose validity
		// probe already passed, so the token is live. A 401 here means this
		// particular door refuses it.
		return CapDenied, fmt.Sprintf("auth rejected (HTTP 401) — the token is live but not accepted to %s "+
			"(%s); %s, don't rotate it", op, refusalCauses(t), refusalRemedy(t))
	case code == 404:
		return CapUnknown, fmt.Sprintf("HTTP 404 probing %q — either the target does not exist yet or this token cannot see it; could not verify", op)
	default:
		return CapUnknown, fmt.Sprintf("unexpected HTTP %d — could not verify authorization", code)
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
func probeOneRoute(t capTransport, token, path, op string) (CapabilityStatus, string, int) {
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
func probeCapability(cc CapContext, c capabilityCheck, token string) CapabilityResult {
	// The grant's consumer is not deployed here, so there is nothing to authorize
	// and nothing to fail on. Reported rather than dropped: an operator reading
	// "not probed" should be able to see WHY the question did not apply.
	if c.component != "" && cc.ComponentOff[c.component] {
		return CapabilityResult{Name: c.token, Op: c.op, Status: CapNotApplicable,
			Detail: "this deployment does not enable the `" + c.component +
				"` component, so the consumer of this grant is never deployed"}
	}
	path, skip := c.path(cc)
	if skip != "" {
		return CapabilityResult{Name: c.token, Op: c.op, Status: CapSkipped, Detail: skip}
	}
	s, d, code := probeOneRoute(c.transport, token, path, c.op)
	if s != CapDenied || c.secondOpinion == nil {
		return CapabilityResult{Name: c.token, Op: c.op, Status: s, Detail: d}
	}

	p2, op2 := c.secondOpinion()
	s2, d2, _ := probeOneRoute(c.transport, token, p2, op2)
	switch s2 {
	case CapOK:
		// THE GRANT IS THERE AND THE ROUTE REFUSED ANYWAY — measured, not inferred.
		// This is the sentence issue #449 wanted something to be able to say: the
		// question `llz ci assert-k8s-version` asks cannot be asked in this pipeline,
		// so that gate is inert here and its green step means nothing. Naming the
		// consequence is the point; a verdict about a credential that the reader has
		// to connect to a gate three steps later is the silence all over again.
		return CapabilityResult{Name: c.token, Op: c.op, Status: CapRouteRefused, Detail: fmt.Sprintf(
			"the token IS authorized to %s, and is refused (HTTP %d) when it tries to %s — same grant, "+
				"different door, so THIS IS THE ROUTE AND NOT THE TOKEN. Nothing to re-scope. "+
				"`llz ci assert-k8s-version` reads that route and warns-and-PASSES when it is refused, so the "+
				"k8sVersion preflight is INERT in this pipeline: its green step is not evidence, and a pin this "+
				"account cannot build will reach `terraform apply` unchecked", op2, code, c.op)}
	case CapDenied:
		// REFUSED AT BOTH, WHICH RULES OUT THE ROUTE AND NOT MUCH ELSE. The overwhelmingly
		// likely cause is the missing grant, and that is what the hint leads with — but
		// two routes under the same API version can also share an explanation that has
		// nothing to do with scope (a v4beta-wide restriction, a suspended account). The
		// verdict is the same either way, because every one of those breaks the cluster
		// apply this preflight runs ahead of; the MESSAGE must not assert the one it
		// cannot distinguish, or an operator spends the afternoon re-scoping a PAT that
		// was never the problem.
		return CapabilityResult{Name: c.token, Op: c.op, Status: CapDenied, Detail: fmt.Sprintf(
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
		// cannot justify. Pasting it in is the same contradiction the CapOK arm above
		// was fixed for, and probeOneRoute returns the code precisely so neither arm
		// has to quote a sentence written for the verdict it is overriding.
		return CapabilityResult{Name: c.token, Op: c.op, Status: CapUnknown, Detail: fmt.Sprintf(
			"refused (HTTP %d) when it tries to %s, and the second opinion could not be taken (%s) — "+
				"so whether this is the token's scope or the route itself is UNRESOLVED. Nothing to act on "+
				"yet; re-run, and if it reproduces the scope line below will say which", code, c.op, d2)}
	}
}

// CapabilityChecksFor reports how many checks are registered for a credential,
// as the ops they probe. It exists so a caller can tell "this credential has no
// scope requirement" (print nothing) from "it has one and we could not ask"
// (print a skip) WITHOUT making a network request to find out.
func CapabilityChecksFor(name string) []string {
	var ops []string
	for _, c := range capabilityChecks {
		if c.token == name {
			ops = append(ops, c.op)
		}
	}
	return ops
}

// CheckCapabilities runs EVERY capability check registered for a credential and
// returns one result per check, in catalog order. An empty slice means the
// credential has no scope requirement to verify — most don't, and the caller
// prints nothing for those.
//
// PLURAL, AND THAT IS THE POINT. This returned the FIRST match and a bool, which
// is a shape that cannot express a credential with two required grants — and
// OPENBAO_SECRETS_WRITE_TOKEN has exactly that: Environments: write for the
// build's own secret writes, and repo-level Secrets: write for the in-cluster
// harbor-robot-provisioner. Registering the second check under the old signature
// would have compiled, run, and been silently unreachable behind the first.
func CheckCapabilities(cc CapContext, name, token string) []CapabilityResult {
	var out []CapabilityResult
	for _, c := range capabilityChecks {
		if c.token == name {
			out = append(out, probeCapability(cc, c, token))
		}
	}
	return out
}

// CapabilityHint returns the remediation text for one denied check. It is keyed
// by (credential, op) rather than credential alone for the same reason
// CheckCapabilities is plural: two checks on one token have two different
// remediations, and printing the first token-matching hint under the second
// check's denial sends the operator to the wrong permission.
func CapabilityHint(name, op string) string {
	for _, c := range capabilityChecks {
		if c.token == name && c.op == op {
			return c.hint
		}
	}
	return ""
}

// CapabilityCell renders one verdict as a colored status + detail (the CI
// report's full-width line).
func CapabilityCell(cr CapabilityResult) string {
	switch cr.Status {
	case CapOK:
		return color.Green("✓ " + cr.Detail)
	case CapDenied:
		return color.Red("✗ DENIED — " + cr.Detail)
	case CapUnknown:
		return color.Yellow("⚠ " + cr.Detail)
	case CapRouteRefused:
		return color.Yellow("⚠ INERT — " + cr.Detail)
	case CapNotApplicable:
		return color.Dim("– n/a: " + cr.Detail)
	default: // CapSkipped
		return color.Dim("– " + orDefault(cr.Detail, "not probed"))
	}
}

// WorstCapability reduces a credential's checks to the one verdict a narrow
// table column can show. Order is by how much the operator needs to act:
// DENIED beats INERT beats UNKNOWN beats SKIPPED beats OK beats NOT-APPLICABLE.
//
// OK RANKS LOWEST, INCLUDING BELOW SKIPPED, and that ordering is the whole point
// rather than a detail. It first read CapSkipped < CapOK, on the reasonable-
// sounding logic that "not probed" is less alarming than "probed and fine" — so a
// credential with one grant verified and another NEVER ASKED (a check whose path
// could not be built) reduced to CapOK and the column printed "✓ scoped". A green
// tick for a question half of which was not put. That is precisely the vacuity
// rule this package's own render gate enforces one case over —
// TestReportReadiness_UnprobedIsNotATick — and a reduction that can manufacture a
// pass out of an unasked question is worse than no column, because it is read as
// evidence. Anything that is not a clean answer must outrank a clean one.
//
// IT REDUCES FOR THE COLUMN ONLY. The full set still prints underneath, because
// a token that is denied one grant and holds another is not described by either
// verdict alone — and the column is where the eye lands, not where the fix is.
func WorstCapability(rs []CapabilityResult) (CapabilityStatus, bool) {
	if len(rs) == 0 {
		return CapSkipped, false
	}
	// NOT-APPLICABLE RANKS BELOW OK so a credential with one grant verified and
	// another that does not apply reads as scoped — everything askable was asked.
	// Only when EVERY check is inapplicable does it reach the column, where it
	// renders as a dim n/a rather than a warning about nothing.
	rank := map[CapabilityStatus]int{CapNotApplicable: 0, CapOK: 1, CapSkipped: 2, CapUnknown: 3, CapRouteRefused: 4, CapDenied: 5}
	worst := rs[0].Status
	for _, r := range rs[1:] {
		if rank[r.Status] > rank[worst] {
			worst = r.Status
		}
	}
	return worst, true
}

// SkipNotSet is the skip detail for a credential that is not configured at all,
// as opposed to one configured somewhere this process cannot read. The
// difference decides whether the readiness report says anything: "not set" is
// already the STATUS column's ✗ missing, and repeating it under every row of a
// fresh instance is noise, while "set on GitHub, not cached" is a fact the
// STATUS column does NOT carry. A shared constant because the producer
// (doctor.ProbeTokenCapabilities) and the renderer (envreq.ReportReadiness) have
// to agree on it, and two spellings of one string is the coupling this repo
// keeps getting bitten by.
const SkipNotSet = "not set"

// AnyStatus reports whether any of a credential's checks reached the given
// verdict. It exists so a caller can tell a WHOLLY unprobed credential from a
// PARTIALLY probed one — WorstCapability deliberately collapses both to
// CapSkipped, and "one grant verified, one unasked" deserves a different word in
// the column than "nothing asked at all".
func AnyStatus(rs []CapabilityResult, want CapabilityStatus) bool {
	for _, r := range rs {
		if r.Status == want {
			return true
		}
	}
	return false
}
