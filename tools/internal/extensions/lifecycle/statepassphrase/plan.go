package statepassphrase

// plan.go — provision TF_STATE_ENCRYPTION_PASSPHRASE, the one
// required secret nothing used to create.
//
// Every Terraform root carries an OpenTofu `encryption` block (ADR 0007 (state encryption)), and the
// shared tf-encryption-env action exits 1 when the passphrase is empty. ADR 0007
// recorded the consequence — "a new required secret on every instance … adopters
// must add it before their next Terraform run" — but that requirement never
// reached `llz tokens`, `llz doctor`'s requirement table, or the quickstart. The
// result was a guaranteed first-build failure for every new adopter: doctor color.Green,
// `llz up` dispatches, and the first Terraform job dies at init. The template's own
// e2e never caught it because the example instance repo had the secret hand-set
// once, so every harness run since has inherited it.
//
// WHY THIS IS NOT JUST ANOTHER ENTRY IN THE WIZARD'S PROMPT LOOP. Overwriting a
// live passphrase makes every existing state file unrecoverable — the same blast
// radius as losing OPENBAO_SEAL_KEY. The wizard's ambient "is it already set?"
// answer cannot be trusted for that decision: ghAPI returns nil on ANY error, so
// envreq.GHSecretNames yields an empty list for a network blip, an expired login or a
// rate limit exactly as it does for a repo with no secrets. Every other credential
// degrades from that to a harmless re-prompt. This one would degrade to
// generate-and-clobber. So this asks GitHub a direct question and acts only on a
// DEFINITE answer: 404 means absent (generate), 200 means present (skip), and
// anything else means unknown — refuse, and say why. Same rule as imagePublished's
// "asked=false must not downgrade the pin" and repoMissing's "a 404 is the only
// answer that means absent".

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// SecretName is the GitHub secret name the Terraform roots read
// through the tf-encryption-env action.
const SecretName = "TF_STATE_ENCRYPTION_PASSPHRASE"

// generateStatePassphrase returns a fresh passphrase.
//
// 32 random bytes in standard base64: 44 characters drawn from [A-Za-z0-9+/=],
// which is inside the [A-Za-z0-9+/=_-] alphabet tf-encryption-env enforces (the
// value is interpolated into an HCL string, so a quote or backslash could inject
// encryption configuration) and comfortably over pbkdf2's 16-character floor.
// This is the same shape as the `openssl rand -base64 32` the action's own error
// message tells operators to run.
func generateStatePassphrase() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state-encryption passphrase: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ghSecretPresent asks whether a secret exists at ONE api path.
//
// Returns (present, answered). answered is false when GitHub did not give a
// verdict — offline, unauthenticated, rate-limited, a 5xx — which callers must
// NOT read as absence. Distinct from liveState.has, which folds every failure
// into "not configured" because its callers only ever re-prompt.
var ghSecretPresent = func(apiPath string) (present, answered bool) {
	_, err := caps.Exec("gh", "api", apiPath, "--silent")
	if err == nil {
		return true, true
	}
	if isNotFoundErr(err) {
		return false, true
	}
	return false, false
}

// statePassphraseExists reports whether the instance already holds a
// state-encryption passphrase in EITHER scope.
//
// Both scopes, because either placement is legal and GitHub resolves the
// env-scoped one FIRST inside an `environment: infra-<env>` job. Checking only
// the repo scope produced a definite 404 for an instance that has the secret on
// infra-<env> — so llz would mint a SECOND, different passphrase, push it
// repo-level, and print a banner claiming it protects state it cannot decrypt.
// Nothing fails at that moment: every environment-scoped job keeps resolving the
// old value, so the pipeline stays green on a passphrase the escrow does not hold.
// The operator escrows the wrong string, and the divergence only becomes data loss
// when the env-scoped copy is removed or a rotation runs. liveState.has and
// ci_token_inventory's gatherSecretAges both already read both scopes for exactly
// this reason.
//
// An indefinite answer on EITHER scope means unknown: a 403 on one of them
// cannot rule out a passphrase living there.
func statePassphraseExists(repo, env string) (present, answered bool) {
	paths := []string{"repos/" + repo + "/actions/secrets/" + SecretName}
	// EVERY infra-* environment, not just the one being provisioned. An instance
	// whose FIRST deployment holds the passphrase on infra-primary would otherwise
	// look empty while provisioning `dr`: both probes 404, llz mints a second
	// passphrase and pushes it repo-level, and the escrowed value now decrypts
	// nothing while infra-primary's own copy still does. Same failure as the
	// single-scope bug, one deployment over. envs falls back to just this one when
	// the listing cannot be read.
	envs, listed := instanceInfraEnvs(repo, env)
	if !listed {
		// The environment LISTING failed (403 on a token with Secrets:read but not
		// Environments:read, a 5xx, offline). Folding that into "only infra-<env>
		// exists" would turn an indefinite answer into a definite absent — exactly
		// what the rule below forbids, and it is how a peer deployment's passphrase
		// becomes invisible.
		return false, false
	}
	for _, e := range envs {
		// 404s when the environment does not exist yet — the normal first-run
		// state, and correctly read as "no env-scoped copy".
		paths = append(paths, "repos/"+repo+"/environments/"+e+"/secrets/"+SecretName)
	}
	for _, p := range paths {
		found, ok := ghSecretPresent(p)
		if !ok {
			return false, false
		}
		if found {
			return true, true
		}
	}
	return false, true
}

// instanceInfraEnvs lists the repo's infra-* GitHub Environments. ok is false
// when the listing could not be read — callers must treat that as UNKNOWN, not as
// "only infra-<env> exists": a passphrase living on a peer deployment's
// environment would otherwise be invisible and a second one minted over it.
//
// --paginate because the endpoint defaults to 30 per page, and an instance past
// 30 environments would silently truncate into the same false-absent.
var instanceInfraEnvs = func(repo, env string) (envs []string, ok bool) {
	want := "infra-" + env
	out := []string{want}
	var listing struct {
		Environments []struct {
			Name string `json:"name"`
		} `json:"environments"`
	}
	if err := caps.GHJSONPaged("repos/"+repo+"/environments", &listing); err != nil {
		return nil, false
	}
	for _, e := range listing.Environments {
		if strings.HasPrefix(e.Name, "infra-") && e.Name != want {
			out = append(out, e.Name)
		}
	}
	return out, true
}

// DropStatePassphraseIfLive removes the passphrase from a push set when the
// instance already holds one, and is the LAST word before any `gh secret set`.
//
// Two entrances needed the same guard. PlanStatePassphrase decides early — before
// the wizard prompts, so a refusal cannot discard freshly-pasted PATs — which
// leaves a window: the repo can acquire a passphrase between that decision and
// the push (a second operator provisioning a peer deployment on the same repo is
// the realistic case). And `llz secrets gather` + `llz secrets push`, the
// documented manual alternative, never consults the plan at all: gather re-prompts
// every catalog entry on every run, its own text says `openssl rand -base64 32`,
// and pushSecrets then pushed whatever was typed straight over the live value.
//
// Re-asking here costs one API call and converts both into a no-op.
func DropStatePassphraseIfLive(repo, env string, secrets map[string]string, minted bool) error {
	if _, ok := secrets[SecretName]; !ok {
		return nil
	}
	present, answered := statePassphraseExists(repo, env)
	if !answered {
		//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation block, not a sentence fragment
		return fmt.Errorf("could not confirm whether %s already exists on %s before pushing it.\n"+
			"  Refusing to Push: overwriting a live passphrase makes every existing Terraform\n"+
			"  state file permanently unreadable. Fix the GitHub lookup and re-run.", SecretName, repo)
	}
	if !present {
		return nil
	}
	delete(secrets, SecretName)
	fmt.Fprintf(os.Stderr, "\n%s %s already exists on %s — NOT overwriting it.\n",
		color.Yellow("!"), SecretName, repo)
	fmt.Fprintln(os.Stderr, "  The repo's copy is the one your existing state is encrypted under; replacing")
	fmt.Fprintln(os.Stderr, "  it would make every state file permanently unreadable. To rotate it properly,")
	fmt.Fprintf(os.Stderr, "  use %s.\n", color.Cyan("secret-rotation.yml (scope: state-passphrase)"))
	if minted {
		// Only reachable if the repo acquired one between plan and push. The
		// operator has just been shown an escrow banner for a value that is now
		// going nowhere; saying so is the difference between a confusing no-op and
		// an escrowed string they believe is live.
		fmt.Fprintf(os.Stderr, "  %s\n", color.Bold("The passphrase printed above was NOT pushed — discard it."))
	}
	return nil
}

// StatePassphrasePlan is what to do about the passphrase this run. Both fields
// are decided BEFORE any prompting or cloud mutation; see PlanStatePassphrase.
type StatePassphrasePlan struct {
	Generate bool // mint a new one (nothing has one yet)
	Push     bool // include it in the `gh secret set` batch
}

// PlanStatePassphrase decides the passphrase's fate from the repo's state and the
// local cache. It runs EARLY — before the wizard prompts for a single PAT or
// creates the state bucket — because its only failure mode is a hard refusal, and
// refusing after the interactive section has already minted an OBJ key and read
// three PATs discards all of that: nothing is persisted until writeEnvFile, so
// the operator re-pastes everything and the first bucket-scoped key is orphaned.
//
// The push decision is the subtle half, and it is a clobber guard in its own
// right. pushToRepo re-pushes every secret in the map unconditionally, so a
// CACHED passphrase would be written over whatever the repo currently holds. That
// is fine while they agree and catastrophic once they do not: `secret-rotation.yml`
// scope=state-passphrase re-keys every root to a new value and then deletes
// TF_STATE_ENCRYPTION_PASSPHRASE_OLD, so a laptop whose .llz/secrets.env still
// holds the pre-rotation value would push it back and strand every state file —
// with no fallback left to read them. And `llz upgrade` routinely sends operators
// back here (`reportCIImageSkew` prints `llz tokens --env <env> --yes` verbatim),
// on a run whose pending re-pin bypasses the "nothing to do" early return.
//
// So: push only what we minted, or a cached value the repo definitively lacks
// (a previous run generated it and the push did not land). Never a cached value
// over a live one — the repo's copy is authoritative, and re-pushing an identical
// value buys nothing anyway.
func PlanStatePassphrase(repo, env string, secrets map[string]string) (StatePassphrasePlan, error) {
	present, answered := statePassphraseExists(repo, env)
	if !answered {
		return StatePassphrasePlan{}, fmt.Errorf("could not determine whether %s already exists on %s.\n"+
			"  Refusing to act: if the secret IS set, overwriting it makes every existing\n"+
			"  Terraform state file permanently unreadable. Fix the GitHub lookup (`gh auth status\n"+
			"  --hostname %s`, connectivity, and note that reading a secret's metadata needs repo\n"+
			"  admin) and re-run — or, if you know the instance has never applied, set it yourself:\n"+
			"      gh secret set %s --repo %s   # openssl rand -base64 32",
			SecretName, repo, ghHost(), SecretName, repo)
	}
	_, cached := secrets[SecretName]
	switch {
	case present:
		return StatePassphrasePlan{}, nil
	case cached:
		return StatePassphrasePlan{Push: true}, nil
	default:
		return StatePassphrasePlan{Generate: true, Push: true}, nil
	}
}

// EnsureStatePassphrase mints the passphrase the plan called for and shows it
// once. Deliberately the LAST thing gathered, so a wizard the operator abandons
// half-way has not created material they never saw the escrow banner for.
// EnsureStatePassphrase writes the state-encryption passphrase into every
// deployment Environment that lacks one.
//
// dryRun AND yes ARE PLAIN BOOLEANS, not package main's globalOpts. They are
// FLAGS, not capabilities — the three-clause rule's second question — and taking
// the whole struct would have pulled main's flag model across the boundary so this
// package could read two bits from it.
func EnsureStatePassphrase(dryRun, yes bool, plan StatePassphrasePlan, repo string, secrets map[string]string) error {
	if !plan.Generate {
		return nil
	}
	if dryRun || !yes {
		fmt.Printf("\n%s %s — %s\n", color.Bold("[state encryption]"), SecretName,
			color.Dim("absent; would generate one and print it once for offline escrow"))
		return nil
	}
	pass, err := generateStatePassphrase()
	if err != nil {
		return err
	}
	secrets[SecretName] = pass
	printStatePassphraseEscrow(repo, pass)
	return nil
}

// printStatePassphraseEscrow shows the generated passphrase once, with the
// consequence of losing it stated at the point the operator can still act.
//
// It goes to stderr and is NOT dimmed: this is the only moment the value is on
// screen, and `llz tokens` output is routinely skimmed. The .llz/secrets.env copy
// is a local cache on one laptop (gitignored, 0600) — it is not escrow, and saying
// so here is the difference between an operator who copies it somewhere durable
// and one who assumes the tooling kept it.
func printStatePassphraseEscrow(repo, pass string) {
	fmt.Fprintf(os.Stderr, "\n%s\n", color.Bold("══ generated "+SecretName+" — COPY IT OFFLINE NOW ══"))
	fmt.Fprintf(os.Stderr, "  %s\n", color.Cyan(pass))
	fmt.Fprintf(os.Stderr, "  %s\n", "The Terraform roots encrypt their state with this (ADR 0007). Lose it and every")
	fmt.Fprintf(os.Stderr, "  %s\n", "state file is permanently unreadable — same blast radius as OPENBAO_SEAL_KEY.")
	fmt.Fprintf(os.Stderr, "  %s\n", color.Dim("Pushed as a repo-level secret on "+repo+" and cached in .llz/secrets.env (0600,"))
	fmt.Fprintf(os.Stderr, "  %s\n", color.Dim("gitignored). Neither is escrow — put it in your secret manager before moving on."))
}
