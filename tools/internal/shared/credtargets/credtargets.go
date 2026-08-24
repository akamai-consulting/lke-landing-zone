// Package credtargets is the TABLE of credentials this platform expects to exist:
// which GitHub secrets and PATs, what each is for, and what state each is supposed
// to be in.
//
// IT CAME OUT OF internal/extensions/tokeninv, which is the thing that BUILDS an
// inventory -- probes the targets, gathers ages, renders a ConfigMap. Four peers
// imported that capability and wanted only this: the target lists, the
// CredExpect/TokenState/SecretProbe vocabularies, and the entry shapes. Knowing
// what SHOULD be there is a different fact from going and looking, and only the
// second is a capability.
//
// THE EXPECT VALUE IS THE PART WITH A RULE IN IT. `absent` is not "we did not
// check" -- it means the API ANSWERED and said 404, which for a credential that is
// supposed to have been revoked is a PASS and for one that is supposed to exist is
// a failure. Every consumer of this table has to make that distinction, which is
// why the vocabulary could not be separated from the targets it labels.
package credtargets

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/credpaths"

// tokenState is the coarse verdict the reconciler turns into llz_token_audit_ok:
// breach → 0 (pages: no-expiry / expired / over-policy / invalid), everything else → 1.
const (
	TokenStateOK      = "ok"      // expiry set and within policy
	TokenStateWarn    = "warn"    // within the warn window (drives lead-time alerts)
	TokenStateBreach  = "breach"  // no-expiry / expired / over-policy / invalid — audit failure
	TokenStateUnknown = "unknown" // not set / unreachable / unparseable — can't verify, don't page
	// TokenStateAbsent — SecretEntry only. The API ANSWERED and said 404: this
	// credential is genuinely not configured. Distinct from `unknown`, which here
	// means the API would not answer (403/5xx) and we therefore know nothing.
	//
	// Collapsing the two is the exact defect this whole ADR-0012 series was
	// written about, one level down. `llz_credential_configured = 0` is read by
	// LLZCredentialUnconfigured as "seed this credential"; publishing it for a
	// credential we merely could not READ turns a token-permission problem into a
	// page that names the wrong thing and sends the operator to the wrong runbook.
	// The risk is not theoretical: the five OpenBao credentials added here are
	// infra-<region> ENVIRONMENT secrets, whose metadata needs different token
	// permissions from the repo-scoped ones, and the probe had never once read an
	// environment-scoped secret in production — it never ran at all.
	TokenStateAbsent = "absent"
)

// TokenEntry is one credential's inventory record. Expiry is unix seconds, 0 when
// unknown or the token never expires (the latter is also state=breach).
type TokenEntry struct {
	Provider string `json:"provider"` // github | linode
	Name     string `json:"name"`
	Expiry   int64  `json:"expiry"`
	State    string `json:"state"`
}

// SecretEntry is one GitHub Actions secret's WRITE time — the age signal for
// credentials that have no expiry to read and cannot live in OpenBao (the
// state-backend key pair, the state-encryption passphrase). Metadata only:
// GitHub Actions secrets are write-only over the API, so there is no value to
// carry even if we wanted to. UpdatedAt is RFC3339; empty means "not configured".
type SecretEntry struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"` // repo | infra-<deployment>
	UpdatedAt string `json:"updated_at,omitempty"`
	Class     string `json:"class"`  // rotation class for the age gauge
	Expect    string `json:"expect"` // present | optional | absent — see credExpect*
	State     string `json:"state"`
}

// Whether a credential is SUPPOSED to be configured. Age is only half of what
// this probe can see: the other half is presence — and presence is NOT uniformly
// good, which is why this is three values and not a bool. One credential's
// healthy state is absent, and two are healthy either way.
const (
	// credExpectPresent — the instance cannot function without it, so a 404 is a
	// finding. Everything except the root token.
	CredExpectPresent = "present"
	// credExpectOptional — legitimately absent on some healthy deployments, so
	// neither presence nor absence is a finding. Measured when present (the age is
	// real and worth seeing); silent when not.
	//
	// This is the Harbor robot pair. `llz ci seed-standby-harbor-robots` returns early
	// with "HARBOR_ROBOT_NAME / HARBOR_PASSWORD not yet published — the active
	// peer's harbor-robot-provisioner CronJob sets them once Harbor is up", so a
	// STANDBY peer, and any deployment before Harbor first comes up, does not have
	// them — by design, not by omission. Classing them `present` (as the first
	// draft of this did) would fire LLZCredentialUnconfigured and FAIL the daily
	// credential job on a perfectly healthy standby.
	CredExpectOptional = "optional"
	// credExpectAbsent — OPENBAO_ROOT_TOKEN. Bootstrap mints a root token, uses
	// it, and REVOKES it (ci_bao_breakglass.go: "a root token is ephemeral by
	// design"); what survives is the 3-of-5 recovery quorum. So a root token
	// sitting in an Actions secret in steady state is a live full-admin
	// credential nobody revoked after a break-glass — the presence IS the
	// finding, and the remedy is `bao-breakglass --action revoke`.
	CredExpectAbsent = "absent"
)

// Inventory is the ConfigMap payload the reconciler reads (data["inventory.json"]).
type Inventory struct {
	Updated int64         `json:"updated"` // unix time the inventory was written (heartbeat)
	Region  string        `json:"region,omitempty"`
	Tokens  []TokenEntry  `json:"tokens"`
	Secrets []SecretEntry `json:"secrets,omitempty"`
	// SecretProbe reports whether the GitHub secrets-metadata probe could run at
	// all: `ok` | `unavailable`. Empty from an inventory written before this
	// field existed, which the reconciler treats as "cannot tell" rather than as
	// either verdict.
	//
	// It exists because an empty Secrets list is AMBIGUOUS and the ambiguity was
	// load-bearing. `Secrets: []` means either "every credential was measured and
	// none exist" or "the probe never ran", and the second is what actually
	// happened for the whole life of ADR 0009's mechanism: the one job that runs
	// `llz ci token-inventory` set OPENBAO_SECRETS_WRITE_TOKEN but never GH_REPO,
	// so newSecretAgeWriter returned an error, the command printed a ::warning::
	// nobody reads, and not one write-time series was ever published. Failing soft
	// was the right call (an instance without a GitHub token still gets its Linode
	// and PAT entries); failing soft INVISIBLY was not.
	SecretProbe string `json:"secret_probe,omitempty"`
}

// Verdicts for Inventory.SecretProbe.
const (
	SecretProbeOK          = "ok"
	SecretProbeUnavailable = "unavailable"
)

// ghSecretTargets are the credentials measured by WRITE TIME rather than expiry.
// They have no expiry (key pairs, passphrases, raw key material) and they cannot
// be tracked the usual way — via an OpenBao `updated_time` — because each one is
// circular with respect to the OpenBao that would hold it: the state-backend
// credentials guard the state that reaches the cluster OpenBao runs in, and the
// seal key, recovery quorum and root token ARE OpenBao's own escrow. Storing any
// of them in OpenBao means losing all of them together. See docs/adr/0009-unmeasurable-credential-coverage.md.
//
// The write-time probe is the mechanism ADR 0009 built for the first three. The
// rest were left behind not because a different mechanism was needed but because
// the target list was a literal nobody revisited — and that is the whole reason
// `llz ci credential-coverage-guard` now exists.
// SecretTarget is one GitHub Actions secret in the coverage catalogue: its name,
// its rotation class, and whether the instance may legitimately lack it.
//
// A NAMED type rather than the anonymous struct this was, because the catalogue
// is consumed outside this package — credential-coverage-guard and
// assert-rotation-health both walk it. That is the point of it: the list of
// credentials the platform expects belongs in ONE place, and the guard that
// checks nothing was forgotten reads the same list the inventory measures.
type SecretTarget struct {
	Name   string
	Class  string
	Expect string
}

var GHSecretTargets = []SecretTarget{
	// ── the state backend (ADR 0009) ──────────────────────────────────────────
	// Operator-dispatchable via secret-rotation.yml scope=tf-state-key.
	{"TF_STATE_ACCESS_KEY", credpaths.CredClassOnDemand, CredExpectPresent},
	{"TF_STATE_SECRET_KEY", credpaths.CredClassOnDemand, CredExpectPresent},
	// Was `static` — correctly, while re-encrypting every state file had no
	// automation. scope=state-passphrase is that automation, so the age is now
	// ACTIONABLE and belongs on the 90d SLA rather than the yearly nudge.
	{"TF_STATE_ENCRYPTION_PASSPHRASE", credpaths.CredClassOnDemand, CredExpectPresent},

	// ── OpenBao's own escrow ─────────────────────────────────────────────────
	//
	// These are the highest-consequence credentials the platform holds and NONE
	// of them was on the single pane. Their absence is not an oversight of degree:
	// the state-backend trio was measured because ADR 0009 went looking for
	// credentials with no expiry, and these have no expiry EITHER — they were
	// simply not in the list it wrote.
	//
	// ASSUMPTION, checked rather than assumed: `expect: present` on these four
	// encodes "OpenBao is deployed". `openbao` is NOT a Mandatory component
	// (clusterspec/components.go marks only argocd and clusterFoundation), so an
	// instance CAN set components.openbao.enabled=false — and there these four are
	// never seeded, so they would page and fail the daily gate.
	//
	// Left as `present` rather than made conditional, because that shape is already
	// unsupported by the job in question: the daily credential run does
	// `alert-eval --strict`, LLZCredentialRotationOverdue names
	// llz_credential_age_days, and the openbao-gauges lane publishes nothing
	// without OpenBao — so --strict already classes it DEAD? and fails, today,
	// before any of this. Building conditional expectation plumbing for a
	// deployment nothing else supports would be machinery for a shape that cannot
	// pass the surrounding checks anyway. Recorded so the next reader knows it was
	// weighed, not missed.
	//
	// OPENBAO_SEAL_KEY is the AES-256 key the chart's `seal "static"` auto-unseal
	// uses (ci_bao_seed_seal_key.go). It is the encryption-at-rest key for
	// everything in OpenBao's raft store, so it is the single credential whose
	// compromise reads every other credential in the platform. `static`: rotating
	// it means a seal rewrap of the whole store, which nothing here implements —
	// so the yearly nudge is the honest signal, not a 90d SLA nobody can meet.
	{"OPENBAO_SEAL_KEY", credpaths.CredClassStatic, CredExpectPresent},
	// The 3-of-5 recovery quorum that authorizes `operator generate-root`. Losing
	// these means break-glass is impossible — which is exactly why an ABSENT one
	// has to be visible (see llz_credential_configured): the failure surfaces on
	// the day you need it and not before.
	{"OPENBAO_RECOVERY_KEY_1", credpaths.CredClassStatic, CredExpectPresent},
	{"OPENBAO_RECOVERY_KEY_2", credpaths.CredClassStatic, CredExpectPresent},
	{"OPENBAO_RECOVERY_KEY_3", credpaths.CredClassStatic, CredExpectPresent},
	// Shares 4 and 5 exist in GitHub only on the FALLBACK path — a first bootstrap
	// dispatched without an escrow public key, where persisting them is the only
	// way values minted exactly once continue to exist. Supply a key and they live
	// solely as ciphertext the operator holds, which is the better posture and the
	// one we want an instance to be able to choose.
	//
	// OPTIONAL, not present, and the distinction is the whole point: both states
	// are correct, so `present` would red every instance that escrowed properly —
	// the false-page shape credExpectOptional exists to avoid — and omitting them
	// would leave the fallback instance's copies unmeasured, which is ADR 0009's
	// original omission repeated one credential over.
	//
	// `static` because nothing rotates them: re-sharding is `bao operator rekey`,
	// which mints an entirely new set rather than lowering these ones' age.
	{"OPENBAO_RECOVERY_KEY_4", credpaths.CredClassStatic, CredExpectOptional},
	{"OPENBAO_RECOVERY_KEY_5", credpaths.CredClassStatic, CredExpectOptional},
	// Expected ABSENT — see credExpectAbsent. `on-demand` because there IS a
	// rotation path (`bao-breakglass --action rotate`); the age matters only in
	// the state this credential is not supposed to be in.
	{"OPENBAO_ROOT_TOKEN", credpaths.CredClassOnDemand, CredExpectAbsent},

	// ── Harbor robots: the standby channel ───────────────────────────────────
	//
	// secret/harbor/robot and secret/harbor/pull-robot are already age-tracked in
	// OpenBao (credpaths.CredPaths, `static`). These are the SECOND copy — published to
	// GitHub by the provisioner so a rebuilt or standby cluster can adopt the
	// existing robots instead of minting new ones (ci_harbor.go's EXISTING_*
	// path). A second copy is a second thing that ages, and nothing was watching
	// it: an OpenBao-side re-seed that failed to republish here leaves the standby
	// channel holding a dead credential, and the OpenBao age would look fine.
	// OPTIONAL, not present — see credExpectOptional. On a standby peer these are
	// published by the ACTIVE peer's provisioner and are absent until it has run.
	{"HARBOR_PASSWORD", credpaths.CredClassStatic, CredExpectOptional},
	{"HARBOR_PULL_PASSWORD", credpaths.CredClassStatic, CredExpectOptional},
}

// The class is the SAME vocabulary the OpenBao age sampler uses
// (reconcile_openbao.go), deliberately: these series are published as
// llz_credential_age_days too, so LLZCredentialRotationOverdue picks them up with
// no rule change. That is also why the class must track reality rather than
// ambition — `on-demand` on a credential with no dispatchable rotation would page
// an operator who has nothing to dispatch. The three state-backend entries have
// one (secret-rotation.yml scopes `tf-state-key` and `state-passphrase`) and so
// does the root token (`bao-breakglass --action rotate`); the seal key, the
// recovery quorum and the Harbor robot copies do NOT, which is precisely why
// they are `static` and draw the yearly nudge instead.

// ghPATTargets declares the GitHub service PATs the inventory measures. It was
// two hardcoded literals at the call site, which is why `E2E_DISPATCH_TOKEN` and
// `GHCR_READ_TOKEN` — both real PATs with a readable expiry — were never on the
// credential single pane at all.
//
// `optional` is the load-bearing field. gatherGitHubTokens classifies an unset
// token as state=unknown rather than dropping it, which is exactly right for a
// PAT every instance must have (its absence is a finding) and exactly wrong for
// one most instances deliberately don't set: a stock instance would grow two
// permanent `unknown` rows on the dashboard, and an inventory that always shows
// unknowns is one nobody reads. Optional targets are therefore skipped when
// unset, and measured identically when present.
// PATTargetSpec is one service PAT in the catalogue.
type PATTargetSpec struct {
	Name     string // the env var, and the `token` label on the metric
	Optional bool
}

var GHPATTargets = []PATTargetSpec{
	// Always expected: the two service PATs bootstrap and apl-core run on.
	{Name: "OPENBAO_SECRETS_WRITE_TOKEN"},
	{Name: "APL_VALUES_REPO_TOKEN"},
	// Template-repo admin only — the e2e harness dispatch PAT. Absent on every
	// adopter instance.
	{Name: "E2E_DISPATCH_TOKEN", Optional: true},
	// Only a PRIVATE fork/image needs a GHCR read credential; the first-party
	// charts are public, so a stock instance leaves it empty by design.
	{Name: "GHCR_READ_TOKEN", Optional: true},
	// The automation PAT template-upgrade.yml runs on: it pushes the upgrade
	// branch and opens its pull request. Needs Workflows: write as well as
	// Contents/Pull-requests — an upgrade rewrites the managed llz-*.yml bodies,
	// and GitHub refuses a PAT push touching a workflow file without it. Opt-in behind a repo variable, so an
	// instance that has not armed it correctly has no such secret: `optional`.
	//
	// IT IS MEASURED RATHER THAN EXEMPT because it is a PAT with a real expiry, and
	// that expiry is silent in the worst possible way — silent to the OPERATOR, not
	// to the run. The workflow fails loudly on a missing or expired token, but it
	// runs monthly, so the red sits in the Actions tab unread and the first symptom
	// anyone actually meets is an instance that has quietly stopped receiving
	// upstream updates, which is indistinguishable from an instance that was
	// already up to date. That is the ADR 0009 shape exactly: not a leak, a
	// credential that fell off the pane.
	{Name: "LLZ_AUTOMATION_TOKEN", Optional: true},
}

// SecretProbeVerdict decides whether the write-time lane can be trusted this
// run. `ok` requires BOTH that the client was built and that every credential
// got an answer.
//
// The second half is the one that is easy to miss. A client that authenticates
// for repo-scoped secrets can still be refused on the environment scope — they
// are different permissions — and a per-credential 403 leaves that credential
// unmeasured while everything else looks healthy. Reporting `ok` there would
// vouch for a lane that is partly dark, which is the failure this field exists
// to make impossible.
func SecretProbeVerdict(clientBuilt bool, secrets []SecretEntry) string {
	if !clientBuilt {
		return SecretProbeUnavailable
	}
	for _, s := range secrets {
		if s.State == TokenStateUnknown {
			return SecretProbeUnavailable
		}
	}
	return SecretProbeOK
}
