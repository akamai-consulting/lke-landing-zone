package onboard

// `llz tokens` provisions the credentials an instance needs to stand up, doing
// the parts the paste-everything wizard (`secrets Gather`) can't: it CREATES the
// Terraform-state OBJ bucket + a bucket-scoped key via the Linode API, gathers
// the GitHub PATs (including the Contents:write APL_VALUES_REPO_TOKEN apl-core's
// otomi.git uses), and writes everything to .llz/*.env so it can be pushed.
//
// It is idempotent: it first reads what's already configured (live repo +
// .llz/*.env), prepopulates variable values, prints the readiness plan (the same
// one `llz doctor` shows), and SKIPS anything already envreq.Satisfied.
//
// Default (adopter) mode targets one instance repo. --admin (maintainer) mode
// additionally wires the template repo's e2e harness and defaults to the example
// repo. Cloud-mutating steps execute only with --yes.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/branchpolicy"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghapi"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/doctor"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

func RunTokens(o Opts, admin bool, env, cluster, bucket, repo string) error {
	deployEnv := env
	if deployEnv == "" {
		if admin {
			deployEnv = "e2e"
		} else {
			return fmt.Errorf("--env is required (e.g. --env primary)")
		}
	}
	if err := validate.EnvName(deployEnv); err != nil {
		return err
	}
	instanceRepo, err := answers.ResolveInstanceRepo(repo, admin)
	if err != nil {
		return err
	}
	if err := ghapi.RequireInstanceRepo(instanceRepo); err != nil {
		return err
	}

	fmt.Printf("%s %s\n", color.Bold("llz tokens"), color.Dim(fmt.Sprintf("— %s (env infra-%s)%s", instanceRepo, deployEnv, adminBanner(admin))))

	if err := os.MkdirAll(".llz", 0o700); err != nil {
		return err
	}
	secrets, vars := envreq.LoadEnvFiles()

	// Discover existing config (instance + template), pull variable VALUES into
	// vars.env, then print the readiness plan (same as `llz doctor`).
	reqs := envreq.E2ERequirements(admin)
	instSt := envreq.FetchLiveState(instanceRepo, deployEnv)
	var tmplSt envreq.LiveState
	if admin {
		tmplSt = envreq.FetchLiveState(templateid.Repo(), "")
	}
	if n := envreq.PrepopulateVars(vars, reqs, instSt, tmplSt); n > 0 {
		fmt.Printf("%s\n", color.Dim(fmt.Sprintf("Prepopulated %d variable value(s) from existing repo config.", n)))
	}
	// PRESENCE isn't CORRECTNESS either, for the two variables llz derives rather
	// than gathers. `llz upgrade` moves the template pin, and TF_IMAGE/KUBE_IMAGE
	// are computed FROM that pin — so an upgraded instance arrives here with both
	// set, both satisfying every check below, and both naming the previous commit.
	// Without this the one operator who has something to do is the one told
	// "nothing to do", and their re-pin has no route that works: the fill at the
	// bottom of this function skips anything already set, by design.
	//
	// A fresh instance costs nothing here — templatecommit.StaleCIImageVars returns before its
	// network round-trips when neither variable is recorded yet.
	repin := templatecommit.StaleCIImageVars(answers.PinnedTemplateRef(), func(k string) string {
		return firstNonEmpty(vars[k], instSt.Value(k))
	})
	for _, s := range repin {
		fmt.Printf("%s %s names an older template commit — re-pinning: %s → %s\n",
			color.Yellow("!"), s.Name, color.Dim(s.Have), color.Cyan(s.Want))
		vars[s.Name] = s.Want
	}
	// Presence isn't validity: actively probe every gathered/cached credential so
	// an expired/revoked/mistyped token surfaces in the VALID column (with "rotate
	// it") instead of 401/403-ing deep in a CI run. Report-only in the wizard.
	ghcrUser := firstNonEmpty(vars["GHCR_USERNAME"], instSt.Value("GHCR_USERNAME"))
	validity, invalidN := doctor.ProbeTokenValidities(reqs, secrets, vars, instSt, ghcrUser)
	// PRESENCE isn't VALIDITY isn't AUTHORIZATION. The third is the one that used
	// to be asked only in CI.
	capabilities, deniedN := doctor.ProbeTokenCapabilities(reqs, secrets, vars, instSt, tokenprobe.CapContext{Repo: instanceRepo, Region: deployEnv, ComponentOff: clusterspec.DisabledComponents(deployEnv)})
	missing := envreq.ReportReadiness(reqs, secrets, vars, instSt, tmplSt, validity, capabilities)
	if invalidN > 0 {
		fmt.Println(color.Dim("  (fix the invalid credential(s) above, then re-run — a dead token fails the CI run later)"))
	}
	if len(missing) == 0 && len(repin) == 0 {
		_ = WriteEnvFile(".llz/vars.env", vars)
		// A DEAD CREDENTIAL IS NOT A GREEN RUN. This branch used to print the ✓ and
		// return nil however the validity probe went, so a revoked-but-present token
		// produced the dim "fix the invalid credential(s)" line, then a green
		// "everything is set", then — because TokensCmd calls PrintNextSteps on a nil
		// return — "Next steps: llz build". Three outputs, the last two contradicting
		// the first, and exit 0 for a tool whose next step cannot work. `llz doctor`
		// already errors in exactly this state (DoctorE2E's `invalid > 0` arm), and
		// `llz up` chains tokens → doctor → build, so the disagreement only bought one
		// more stage before the same stop.
		//
		// ONLY ON THIS PATH, deliberately. invalidN is measured before the interactive
		// section; past this point the operator may have just pasted a replacement for
		// the very token that probed dead, and failing on a stale measurement would
		// reject the fix. Here nothing was prompted, so the measurement still holds.
		if err := CredentialRefusal(invalidN, deniedN, instanceRepo); err != nil {
			return err
		}
		// PRESENCE was satisfied; the question left is whether what CI holds is
		// still what you have. Nothing above can answer it — the table's VALID and
		// PERMS columns probe the LOCAL copy, because GitHub never reads a secret
		// back — so this is the last chance to say so before the operator goes and
		// dispatches a build against a value nobody checked.
		drift := envreq.DetectDrift(reqs, secrets, vars, instSt)
		fmt.Printf("\n%s %s\n", color.Green("✓"), NothingToProvisionNote(deployEnv, instanceRepo, drift))
		if note := drift.Note(deployEnv, instanceRepo); note != "" {
			fmt.Print(note)
		}
		return OfferStalePush(o, instanceRepo, deployEnv, secrets, instSt, drift)
	}
	if o.DryRun {
		fmt.Printf("\n%s\n", color.Dim(fmt.Sprintf("(dry-run) would provision the %d missing REQUIRED item(s) above%s.",
			len(missing), RepinPlanNote(repin))))
		return nil
	}
	if !o.Yes {
		fmt.Println("\n" + color.Dim("(no --yes: will Gather + write .llz/*.env + print the push plan, but create/write nothing)"))
	}

	// Decide the state-encryption passphrase's fate BEFORE the interactive section
	// below: its only failure mode is a hard refusal, and refusing after the
	// prompts would throw away three freshly-pasted PATs and orphan a
	// bucket-scoped OBJ key this run created. See state_passphrase.go.
	passPlan, err := statepassphrase.PlanStatePassphrase(instanceRepo, deployEnv, secrets)
	if err != nil {
		return err
	}

	// have(name) — already envreq.Satisfied (env file or live instance repo) → skip.
	have := func(name string, secret bool) bool {
		return envreq.Satisfied(envreq.Requirement{Name: name, Secret: secret}, secrets, vars, instSt)
	}
	in := bufio.NewScanner(os.Stdin)

	// ── Linode: PAT + state bucket + scoped key ──────────────────────────────
	needKeys := !have("TF_STATE_ACCESS_KEY", true) || !have("TF_STATE_SECRET_KEY", true)
	clusterID := clusterFromEndpoint(vars["TF_STATE_ENDPOINT"])
	if needKeys || !have("LINODE_API_TOKEN", true) {
		fmt.Printf("\n%s API token — full Read/Write (provisioning; also creates the state bucket)\n", color.Bold("[Linode]"))
		openURL(o, linodeTokensURL)
		fmt.Printf("      %s %s\n", color.Dim("create at:"), color.Cyan(linodeTokensURL))
		token := cli.Prompt(in, "Linode PAT")
		if token == "" {
			return fmt.Errorf("a Linode PAT is required")
		}
		if !have("LINODE_API_TOKEN", true) {
			secrets["LINODE_API_TOKEN"] = token
		}
		if needKeys {
			// Fenced: the wizard READS the account to let an operator pick a
			// cluster and to mint nothing. See capability.ReadOnlyCloud.
			client := capability.ReadOnlyCloud().Client(token, 30*time.Second)
			ctx := context.Background()
			if clusterID == "" {
				clusterID = cluster
			}
			if clusterID == "" {
				if clusterID, err = pickCluster(ctx, client, in); err != nil {
					return err
				}
			}
			vars["TF_STATE_ENDPOINT"] = "https://" + clusterID + ".linodeobjects.com"
			bucketName := firstNonEmpty(bucket, vars["TF_STATE_BUCKET"], repoSlug(instanceRepo)+"-tfstate")
			vars["TF_STATE_BUCKET"] = bucketName
			fmt.Printf("%s state bucket %q in %s\n", color.Bold("[Linode]"), bucketName, clusterID)
			if o.Yes {
				if _, err := client.CreateObjectStorageBucket(ctx, clusterID, bucketName); err != nil {
					return fmt.Errorf("create bucket: %w", err)
				}
				// Name what earlier interrupted runs left behind before adding to it
				// (tokens_statekey.go). Report-only — the repo still reads one of them
				// until this run's push lands.
				reportOrphanedStateKeys(ctx, client, instanceRepo)
				key, err := client.CreateObjectStorageKey(ctx, stateKeyLabel(instanceRepo), clusterID, bucketName, "read_write")
				if err != nil {
					return fmt.Errorf("create scoped key: %w", err)
				}
				ak, _ := key["access_key"].(string)
				sk, _ := key["secret_key"].(string)
				if ak == "" || sk == "" {
					return fmt.Errorf("create-key response missing access_key/secret_key")
				}
				secrets["TF_STATE_ACCESS_KEY"], secrets["TF_STATE_SECRET_KEY"] = ak, sk
				fmt.Printf("      %s bucket + scoped read_write key created\n", color.Green("✓"))
				// Persist the moment the credential exists, not at the end of the
				// wizard. Everything between here and the push is interactive — three
				// PAT prompts the operator may well Ctrl-C out of to go create one —
				// and the secret half of an OBJ key is shown exactly ONCE. Deferring
				// the write meant an interrupt left a live read_write key on the state
				// bucket with no record anywhere, and the re-run (finding nothing
				// cached) minted another. Nothing reaps `llz-tfstate-*`, so they
				// accumulated. checkpointEnvFiles is best-effort: the authoritative
				// write still happens below.
				checkpointEnvFiles(secrets, vars)
			} else {
				fmt.Println(color.Dim("      (--yes to create the bucket + scoped key)"))
			}
		}
	} else {
		fmt.Println("\n" + color.Bold("[Linode]") + color.Dim(" token + state bucket/key already set — skipping"))
	}

	// ── GitHub PATs ──────────────────────────────────────────────────────────
	// gatherGH prompts for one PAT. It opens + prints the primary minting link;
	// when altURL is non-empty it also prints an alternate option (e.g. classic
	// vs fine-grained) so the operator can pick whichever their org policy allows.
	gatherGH := func(name, note, primaryLabel, primaryURL, altLabel, altURL string) {
		if have(name, true) {
			fmt.Printf("%s %s\n", color.Bold("[GitHub]"), color.Dim(name+" already set — skipping"))
			return
		}
		openURL(o, primaryURL)
		fmt.Printf("\n%s %s — %s\n", color.Bold("[GitHub]"), name, note)
		fmt.Printf("      %s:\n        %s\n", primaryLabel, color.Cyan(primaryURL))
		if altURL != "" {
			fmt.Printf("      %s:\n        %s\n", color.Dim(altLabel), color.Cyan(altURL))
		}
		if v := cli.Prompt(in, name); v != "" {
			secrets[name] = v
		}
	}
	owner, _, _ := strings.Cut(instanceRepo, "/")
	// OPENBAO_SECRETS_WRITE_TOKEN: two consumers, two grants. CI's `gh secret
	// set` persists the OpenBao unseal keys into the infra-<env> ENVIRONMENT
	// (Environments: write — "Secrets" does not cover environment secrets), and
	// the in-cluster harbor-robot-provisioner publishes the REPO-level HARBOR_*
	// secrets with the same PAT (Secrets: write — Environments does not cover
	// those). Neither implies the other; see ghFineGrainedSecretsWriteURL, whose
	// pre-fill requests both. A classic repo+workflow PAT carries all three —
	// offer both. Either way the PAT owner must be Environment admin on every
	// infra-<env> environment, or the --env-scoped writes 401.
	//
	// THIS LABEL IS THE FIFTH COPY of that advice and was the last one still
	// saying "Environments: write" alone, after the URL pre-fill, the catalog
	// Note, the workflow require-secret hints and the docs had all been corrected
	// together. It is what an operator READS while pasting the token, so of the
	// five it is the worst one to leave stale.
	gatherGH("OPENBAO_SECRETS_WRITE_TOKEN",
		"CI persists OpenBao unseal keys into the infra-<env> environment (you must also be Environment admin on it)",
		secretsWritePATLabel(instanceRepo),
		ghFineGrainedSecretsWriteURL("llz-openbao-secrets-write", owner),
		"classic (scopes repo + workflow)",
		ghTokenURL("repo,workflow", "llz-openbao-secrets-write"))
	// HARD-required by terraform apply: apl-core's otomi.git.password + the
	// argocd repo Secrets. apl-operator PUSHES its values tree to this repo, so
	// the PAT needs Contents: write (the in-cluster Gitea is obsoleted). The
	// template URL pre-fills name/owner/Contents:write; GitHub can't pre-select
	// the specific repo, so the note tells the operator to pick it.
	gatherGH("APL_VALUES_REPO_TOKEN",
		"apl-core values repo (otomi.git) + argocd repo Secrets; apl-operator PUSHES its values tree here",
		"fine-grained (Contents: write pre-filled; Only select repositories: "+instanceRepo+")",
		ghFineGrainedTokenURL("llz-apl-values-repo", owner, "apl-core values repo (otomi.git) + argocd repo Secrets"),
		"", "")
	// (The template repo + its first-party modules are public, so no TEMPLATE_TOKEN
	// is needed — the reusable workflows check it out anonymously.)
	// (The first-party OCI Helm charts under ghcr.io/<org>/charts are public, so
	// ArgoCD pulls them anonymously — GHCR_READ_TOKEN stays EMPTY for a stock
	// instance. A private fork / private image can set it; it's now a tracked
	// OPTIONAL pair gathered in the optional-secrets section below, not hand-set,
	// so `llz doctor` shows + validates it and a stale PAT can't silently rot.)

	// ── Computed vars ────────────────────────────────────────────────────────
	// Gated on actually having something to compute. computeCIImageVars makes up to
	// five network requests and can print a warning about a fallback, and this
	// command's headline property is that a re-run "SKIPS anything already
	// envreq.Satisfied" — so doing that work for two variables it is not going to touch
	// both slows the idempotent path and, worse, warns that TF_IMAGE/KUBE_IMAGE are
	// unpinned when they are already set to something the operator chose.
	needTF, needKube := !have("TF_IMAGE", false), !have("KUBE_IMAGE", false)
	if needTF || needKube {
		templatecommit.ComputeAndReportImageVars(vars, needTF, needKube)
	}

	// ── Optional secrets ─────────────────────────────────────────────────────
	// (CLOUD_FIREWALL_TOKEN was retired: the firewall-controller token is now
	// ESO-synced from OpenBao's secret/linode/api-token via the cidrFirewall
	// component, so there is no GH-secret consumer left.)
	for _, s := range []struct{ name, desc string }{
		{"LINODE_DNS_TOKEN", "Linode token, Domains: Read/Write (cert-manager DNS-01)"},
	} {
		if have(s.name, true) {
			continue
		}
		fmt.Printf("\n%s %s — %s\n", color.Bold("[optional]"), s.name, color.Dim(s.desc))
		if v := cli.Prompt(in, s.name+" (Enter to skip)"); v != "" {
			secrets[s.name] = v
		}
	}

	// ── Optional GHCR read credential (private fork / private image only) ──────
	// The stock first-party charts are PUBLIC, so leave this EMPTY — do NOT set a
	// token you don't need: a stale GHCR PAT 403s the chart pre-flight (the
	// ghcrChartPublished anonymous fallback now rides that out, but empty is still
	// the clean default). Gathered as a pair: the read:packages secret + its owner
	// variable; the username is only meaningful alongside the token.
	if !have("GHCR_READ_TOKEN", true) {
		fmt.Printf("\n%s GHCR_READ_TOKEN — %s\n", color.Bold("[optional]"),
			color.Dim("GitHub read:packages PAT — ONLY for a private fork or private image; Enter to skip (public charts pull anonymously)"))
		if v := cli.Prompt(in, "GHCR_READ_TOKEN (Enter to skip)"); v != "" {
			secrets["GHCR_READ_TOKEN"] = v
			if !have("GHCR_USERNAME", false) {
				if u := cli.Prompt(in, "GHCR_USERNAME (owner of that PAT)"); u != "" {
					vars["GHCR_USERNAME"] = u
				}
			}
		}
	}

	// ── state-encryption passphrase ──────────────────────────────────────────
	// Generated, not prompted: it is machine material with no issuer to visit, and
	// every Terraform root refuses to run without it.
	if err := statepassphrase.EnsureStatePassphrase(o.DryRun, o.Yes, passPlan, instanceRepo, secrets); err != nil {
		return err
	}

	// ── persist + push ───────────────────────────────────────────────────────
	if err := WriteEnvFile(".llz/secrets.env", secrets); err != nil {
		return err
	}
	if err := WriteEnvFile(".llz/vars.env", vars); err != nil {
		return err
	}
	// AFTER the cache is written, so the operator keeps their local copy, and
	// BEFORE the push, which re-sets every secret in the map unconditionally. The
	// repo's passphrase is authoritative once it exists: pushing a cached one over
	// it is a no-op at best and, against a rotated repo, strands every state file.
	// Re-asked here rather than trusting passPlan, because the plan was made before
	// the interactive section and the repo can have acquired a passphrase since.
	if err := statepassphrase.DropStatePassphraseIfLive(instanceRepo, deployEnv, secrets, passPlan.Generate); err != nil {
		return err
	}
	nSecrets, nVars := len(secrets), len(vars)
	fmt.Printf("\n%s wrote %d secret(s) + %d variable(s) to .llz/\n", color.Green("✓"), nSecrets, nVars)

	// Admin e2e harness (template-repo vars + E2E_DISPATCH_TOKEN) runs BEFORE the
	// instance-repo push: a push / branch-policy failure on the instance repo
	// must not suppress the E2E_DISPATCH_TOKEN creation link the maintainer needs.
	if admin {
		if err := configureTemplateHarness(o, in, instanceRepo, clusterID, tmplSt); err != nil {
			return err
		}
	}
	if err := pushToRepo(o, instanceRepo, deployEnv, secrets, vars, instSt); err != nil {
		return err
	}
	if !o.Yes {
		fmt.Println("\n" + color.Dim("(no --yes: nothing was created or pushed — re-run with --yes to execute)"))
	}
	return nil
}

// PrintNextSteps prints the recommended flow after a real `llz tokens` run
// (credentials provisioned + pushed). Only the standalone command calls it — the
// `llz up` chain runs tokens → doctor → build itself and prints its own guidance.
func PrintNextSteps(env string) {
	const col = 26
	cmd := func(c, note string) {
		pad := col - len(c)
		if pad < 2 {
			pad = 2
		}
		fmt.Printf("  %s%s%s\n", color.Cyan(c), strings.Repeat(" ", pad), color.Dim("# "+note))
	}
	fmt.Println("\n" + color.Bold("Next steps"))
	cmd("llz doctor --env "+env, "confirm every required value is set")
	cmd("llz build "+env+" --yes", "dispatch the apply  (or `llz up "+env+" --yes` chains doctor → build)")
	cmd("llz status "+env, "watch OpenBao / ArgoCD / ESO converge")
	fmt.Println(color.Dim("  after the first build: escrow the OpenBao recovery shares offline (only possible if"))
	fmt.Println(color.Dim("  the build was dispatched with openbao_escrow_pubkey_b64 — nothing is printed in the clear),"))
	fmt.Println(color.Dim("  and delete OPENBAO_ROOT_TOKEN from infra-" + env + " (DNS-01 certs wire automatically via TF_VAR_linode_dns_token)."))
}

// DoctorE2E reports e2e readiness of the env files + live repo (the wizard's
// plan, runnable standalone). Wired as `llz doctor` (see cmdDoctor).
// tokensCommand is the provisioning command this report tells operators to run.
//
// ONE CONSTRUCTION, TWO PRINTERS, because they were two and they disagreed: the
// missing-items line resolved the deployment while the ci-image line four lines
// above it printed `llz tokens --env <env> --yes`, placeholder intact. Adjacent
// in the same report, so the tool contradicted itself on screen — and the
// placeholder one is the instruction that leads the post-upgrade checklist.
func tokensCommand(env string, admin bool) string {
	return "llz tokens" + adminFlag(admin) + " --env " + env + " --yes"
}

func DoctorE2E(repo, env string, admin bool) error {
	instanceRepo, err := answers.ResolveInstanceRepo(repo, admin)
	if err != nil {
		return err
	}
	if env == "" {
		env = "e2e"
	}
	if err := ghapi.RequireInstanceRepo(instanceRepo); err != nil {
		return err
	}
	secrets, vars := envreq.LoadEnvFiles()
	reqs := envreq.E2ERequirements(admin)
	instSt := envreq.FetchLiveState(instanceRepo, env)
	var tmplSt envreq.LiveState
	if admin {
		tmplSt = envreq.FetchLiveState(templateid.Repo(), "")
	}
	fmt.Printf("\n%s\n", color.Bold(fmt.Sprintf("e2e readiness — %s (infra-%s)%s", instanceRepo, env, adminBanner(admin))))
	// Actively probe validity, not just presence — a set-but-dead token is the
	// failure that otherwise only shows up as a 401/403 mid-CI-run.
	ghcrUser := firstNonEmpty(vars["GHCR_USERNAME"], instSt.Value("GHCR_USERNAME"))
	validity, invalid := doctor.ProbeTokenValidities(reqs, secrets, vars, instSt, ghcrUser)
	// And SCOPE — the question that authenticates cleanly and still 403s. Probed
	// against this deployment (repo + infra-<env>), so doctor asks exactly what
	// `llz ci validate-tokens` asks from inside the run it is standing in front of.
	capabilities, denied := doctor.ProbeTokenCapabilities(reqs, secrets, vars, instSt, tokenprobe.CapContext{Repo: instanceRepo, Region: env, ComponentOff: clusterspec.DisabledComponents(env)})
	missing := envreq.ReportReadiness(reqs, secrets, vars, instSt, tmplSt, validity, capabilities)
	// PRESENCE is not FRESHNESS. reportReadiness ticks TF_IMAGE/KUBE_IMAGE as set;
	// `llz ci assert-image-fresh` — the first step of the apply's first job —
	// additionally requires them to name THIS instance's pin. Same merged lookup
	// `llz tokens` re-pins from, so doctor sees what CI will see.
	pinErr := checkCIImagePins(tokensCommand(env, admin), func(k string) string {
		return firstNonEmpty(vars[k], instSt.Value(k))
	})
	if len(missing) > 0 {
		fmt.Printf("\n%s %d required item(s) missing: %s\n", color.Red("✗"), len(missing), strings.Join(missing, ", "))
		fmt.Println("  run `" + tokensCommand(env, admin) + "` to provision them.")
	}
	if err := CredentialRefusal(invalid, denied, instanceRepo); err != nil {
		return err
	}
	if pinErr != nil {
		return pinErr
	}
	// Missing REQUIRED config has to fail, not just print.
	//
	// This reported "✗ N required item(s) missing" and then returned nil, so
	// `llz doctor --env <env>` exited 0 on an instance that cannot build — while
	// its own documentation says "Green when every required item is set". Two ways
	// that bites: an operator reads exit 0 as ready and runs `llz build`, and
	// `llz up --skip-tokens` (which the quickstart documents) walks straight past
	// stage 2 into the dispatch. Either way the answer arrives from CI's
	// `require-secret` minutes later instead of from the gate that exists to
	// prevent exactly that.
	//
	// reportReadiness collects REQUIRED items only (state.go: `if r.Required &&
	// !onGitHub`), so an optional credential left unset still exits 0.
	if len(missing) > 0 {
		return fmt.Errorf("%d required item(s) not set on %s: %s — run `llz tokens%s --env %s --yes`",
			len(missing), instanceRepo, strings.Join(missing, ", "), adminFlag(admin), env)
	}
	fmt.Println("\n" + color.Green("✓") + " ready — every required value is set, and every probeable token is valid and scoped for its job.")
	return nil
}

func adminFlag(admin bool) string {
	if admin {
		return " --admin"
	}
	return ""
}

// ── helpers ──────────────────────────────────────────────────────────────────

func adminBanner(admin bool) string {
	if admin {
		return " [ADMIN: + " + templateid.Repo() + " e2e harness]"
	}
	return ""
}

func repoSlug(repo string) string {
	if _, name, ok := strings.Cut(repo, "/"); ok {
		return strings.ToLower(name)
	}
	return strings.ToLower(repo)
}

// InvalidCredentialsError is the single refusal `llz tokens` and `llz doctor`
// both return when the validity probe found present-but-dead credentials. nil
// when there are none, so callers can `if err := ...; err != nil`.
//
// ONE FUNCTION BECAUSE THE TWO COMMANDS DISAGREED. `llz doctor` errored on
// invalid > 0; `llz tokens`, on the path where nothing is missing, printed the
// dim "fix the invalid credential(s)" line and then returned nil anyway — which
// TokensCmd reads as success and answers with "Next steps: llz build". Same
// probe, same instance, same moment, opposite verdicts, and the one that exits 0
// is the one an operator runs first. Restating the rule in both places is what
// let them drift; there is now one place to change and one place to test.
func InvalidCredentialsError(n int, repo string) error {
	if n <= 0 {
		return nil
	}
	return fmt.Errorf("%d probeable credential(s) on %s are invalid — rotate them (see the validity report above)", n, repo)
}

// DeniedCredentialsError is the refusal for a credential that AUTHENTICATES and
// is not authorized for the operation it exists to perform. nil when there are
// none.
//
// SEPARATE FROM InvalidCredentialsError BECAUSE THE REMEDY IS THE OPPOSITE ONE.
// An invalid token is dead and must be rotated; a denied token is alive, in date,
// and under-scoped — rotating it produces an identically under-scoped replacement
// and burns the afternoon. The messages must not be interchangeable, so neither
// are the errors.
//
// n counts CREDENTIALS, not refused grants: one PAT can be denied several and is
// still one thing to go and fix.
func DeniedCredentialsError(n int, repo string) error {
	if n <= 0 {
		return nil
	}
	return fmt.Errorf("%d required credential(s) on %s authenticate but lack a required permission — RE-SCOPE them, do not rotate (see the scope notes above)", n, repo)
}

// secretsWritePATLabel is the on-screen choice an operator reads while pasting
// OPENBAO_SECRETS_WRITE_TOKEN — the fifth copy of this credential's permission
// advice, and the one that was last to be corrected.
//
// A NAMED FUNCTION SO A GATE CAN SEE IT. It was an inline literal in a call
// three frames deep, which is reachable by grep and by review and by nothing
// else; the drift class it belongs to has now been caught twice by human reading
// and never by CI. Hoisting it costs one function and puts it inside
// pat_guidance_drift_test.go's reach with the other four.
func secretsWritePATLabel(instanceRepo string) string {
	return "fine-grained, recommended (Actions: write + Environments: write + Secrets: write; " +
		"Only select repositories: " + instanceRepo + ")"
}

// CredentialRefusal is the ONE decision both `llz tokens` and `llz doctor` make
// about a probe result: does what we just measured stop the operator here?
//
// IT IS ONE FUNCTION BECAUSE THE SECOND VERDICT REPEATED THE FIRST ONE'S BUG.
// InvalidCredentialsError exists because the two verbs disagreed about a dead
// token — doctor errored, tokens printed a green "nothing to provision" and
// "Next steps: llz build" — and its own comment names the cause: "Restating the
// rule in both places is what let them drift; there is now one place to change
// and one place to test." When the scope probe was added, doctor was wired to
// stop on a denial and tokens was not, so a PAT rendering "PERMS ✗ DENIED"
// reached the same green line and the same exit 0. Two verdicts, one rule, same
// drift, one release apart.
//
// WHAT THE SINGLE FUNCTION DOES AND DOES NOT BUY. It buys one definition of the
// rule and one test of it, and it makes ADDING a probe safe: a new parameter here
// is a compile error at both call sites, so the next verdict cannot be wired into
// one verb and forgotten in the other. It does NOT prevent the slip that actually
// happened — `CredentialRefusal(invalidN, 0, repo)` compiles perfectly, and a
// caller that stops threading a real count through is still invisible to the
// compiler and to these tests, which exercise the rule and not the wiring.
// Nothing here relieves a reader of checking that both call sites pass measured
// numbers; claiming otherwise would put a guarantee in a comment that the code
// does not carry, which is its own version of the bug above.
//
// Invalidity is reported FIRST when both are present: a dead credential cannot be
// scope-probed meaningfully, so "rotate it" is the instruction that unblocks, and
// leading with "re-scope" would have the operator re-mint a token that is about to
// be replaced anyway.
func CredentialRefusal(invalid, denied int, repo string) error {
	if err := InvalidCredentialsError(invalid, repo); err != nil {
		return err
	}
	return DeniedCredentialsError(denied, repo)
}

// NothingToProvisionNote is what `llz tokens` says when it finds every required
// credential already accounted for — and, load-bearingly, what it says NEXT.
//
// THE OLD TEXT WAS "Everything required for e2e is already set — nothing to
// do.", and it misled the one operator most likely to reach it, twice.
//
// It named `e2e` no matter what --env was passed — the template's own throwaway
// lane, not the adopter's deployment. That is the same misdirection
// DefaultDoctorEnv() exists to remove one verb over (see cobra_root2.go), landing
// here because this string was a constant rather than a format.
//
// The worse half is "nothing to do", which is a claim about THIS COMMAND that
// reads as a claim about the REPO. The readiness behind it is envreq.Satisfied,
// and that tests PRESENCE — a key in .llz/secrets.env, or a secret NAME on the
// instance repo — never VALUE, because GitHub does not disclose a secret back.
// So an operator who rotates a credential the obvious way, by editing
// .llz/secrets.env, arrives here with everything "satisfied", is told there is
// nothing to do, and RETURNS BEFORE pushToRepo. Their new value stays on their
// laptop, CI keeps authenticating with the old one, and the failure surfaces
// later as a 401 from a build that had no reason to be reading a stale token.
// The command that would have shipped it — `llz secrets push` — is a sibling
// verb nothing in this output mentions, so the operator has to already know the
// answer to find it.
//
// Detecting the drift instead of announcing the route is not available: the
// comparison that would find it is local value vs pushed value, and the pushed
// half is precisely what the API withholds. Naming the command out loud is the
// whole of the remedy, which is why it is unconditional here rather than gated
// on some heuristic about whether the operator edited anything.
//
// IT NAMES THE CHECKOUT, NOT JUST THE COMMAND, because `llz secrets push` and
// this command do not resolve their target the same way. RunTokens honours
// --repo / .copier-answers.yml; PushSecrets builds its argv through
// ghcli.SecretSetArgv / ghcli.VariableSetArgv, neither of which passes --repo, so
// it pushes wherever `gh` infers the repo from the working directory. The two
// agree for the ordinary case — an adopter in their instance checkout — and
// diverge exactly when this note would otherwise be most confident: a --repo run,
// and every --admin run, where RunTokens targets the example instance while the
// working directory is the template. Advice that sends a full set of credentials
// into the wrong repo is worse than no advice, and repeating the repo here is
// what makes the sentence true in both modes.
//
// Both env files, not just secrets.env: this early return skips pushToRepo
// entirely, so a hand-edited variable is dropped on the same floor. (Variables
// alone WOULD be detectable — pushToRepo compares st.Value(k) != vars[k] — but it
// never runs on this path, and `llz secrets push` re-pushes both files anyway.)
//
// AND IT WAS STILL WALLPAPER. The paragraph below was printed unconditionally —
// identical output whether the instance was perfectly in sync or two minutes from
// an outage — so it carried no information and was read as boilerplate. In run
// 33556210825 it was on screen, correct, and ignored, because it is always on
// screen. A warning that cannot be absent cannot be a warning.
//
// It now says which of the two states it found. Drift.Note carries the specifics
// when there are any; this keeps the caveat only for the case where the metadata
// comparison could not be made at all, and otherwise states the stronger fact the
// comparison earns.
func NothingToProvisionNote(env, repo string, d envreq.Drift) string {
	head := fmt.Sprintf("Everything required for infra-%s is already set — nothing to provision.", env)
	switch {
	case !d.Empty():
		// The specifics follow immediately; a generic caveat here would only
		// dilute them.
		return head
	case d.LocalMod.IsZero():
		return head + "\n" + color.Dim(fmt.Sprintf(
			"  This checks that each credential is PRESENT, not that its pushed value still matches\n"+
				"  yours — GitHub never reads a secret back, and there is no local %s to date\n"+
				"  them against. If you hold newer values elsewhere, send them with\n"+
				"  `llz secrets push %s --yes` from a checkout of %s.", envreq.SecretsEnvFile, env, repo))
	default:
		return head + "\n" + color.Dim(fmt.Sprintf(
			"  Every pushed secret was written at or after your last edit to %s, and every\n"+
				"  variable matches. Values still cannot be compared — GitHub never reads a secret\n"+
				"  back — but nothing you hold locally is newer than what CI has.", envreq.SecretsEnvFile))
	}
}

// OfferStalePush asks whether to re-push the secrets whose pushed copy predates
// the local file, and pushes ONLY those.
//
// Only the Behind set, never the whole file. `llz ci rotate-broad-pat` publishes a
// fresh LINODE_API_TOKEN to every infra-<deployment> and revokes the one it
// replaced, and llz-secret-rotation.yml does the same for the TF_STATE_* pair —
// so a blanket re-push is how you overwrite three live credentials with revoked
// ones from a command that then reports success. Drift.Ahead is exactly that set,
// and it is excluded here by construction.
//
// Gated on a TTY as well as --yes: this writes to a live environment, and an
// unanswerable prompt must not become an unattended push. Without one the printed
// `llz secrets push` command is the whole answer.
func OfferStalePush(o Opts, repo, env string, secrets map[string]string, st envreq.LiveState, d envreq.Drift) error {
	if len(d.Behind) == 0 || o.DryRun || !o.Yes || !cli.Interactive() {
		return nil
	}
	names := d.BehindNames()
	fmt.Printf("\n%s\n", color.Bold(fmt.Sprintf("Re-push the %d secret(s) above to infra-%s?", len(names), env)))
	fmt.Printf("%s\n", color.Dim("  Only those — the ones CI rotated after your last edit are left alone."))
	in := bufio.NewScanner(os.Stdin)
	if ans := strings.ToLower(cli.Prompt(in, "push now? [y/N]")); ans != "y" && ans != "yes" {
		fmt.Printf("%s\n", color.Dim(fmt.Sprintf("  Skipped. Push them later with `llz secrets push %s --yes`.", env)))
		return nil
	}
	stale := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := secrets[n]; ok {
			stale[n] = v
		}
	}
	// No vars: VarsDiffer is reported for the operator to resolve deliberately,
	// and a variable is readable, so it needs no rescue by timestamp.
	return pushToRepo(o, repo, env, stale, nil, st)
}

// RepinPlanNote is the dry-run tail that keeps a repin-only run from reporting
// "0 missing REQUIRED item(s)" and nothing else — which reads as "no work" for
// precisely the run that has some.
func RepinPlanNote(repin []templatecommit.ImageSkew) string {
	if len(repin) == 0 {
		return ""
	}
	return fmt.Sprintf(" and re-pin %d ci image variable(s) to the current template pin", len(repin))
}

// clusterFromEndpoint: https://us-ord-1.linodeobjects.com -> us-ord-1.
// A THIN ALIAS, because this was one of TWO implementations of the same rule and
// both were wrong on the virtual-host spelling — this one returning
// "<bucket>.us-ord-1" where the cluster is "us-ord-1". See
// linode.ObjClusterFromEndpoint.
func clusterFromEndpoint(endpoint string) string {
	return linode.ObjClusterFromEndpoint(endpoint)
}

// regionFromCluster strips the trailing cluster ordinal: us-ord-1 -> us-ord.
func regionFromCluster(clusterID string) string {
	if i := strings.LastIndex(clusterID, "-"); i > 0 {
		return clusterID[:i]
	}
	return clusterID
}

func pickCluster(ctx context.Context, client *linode.Client, in *bufio.Scanner) (string, error) {
	clusters, err := client.ListObjectStorageClusters(ctx)
	if err != nil {
		return "", fmt.Errorf("list OBJ clusters: %w", err)
	}
	fmt.Println("\n  " + color.Bold("Object Storage clusters:"))
	for _, c := range clusters {
		id, _ := c["id"].(string)
		region, _ := c["region"].(string)
		status, _ := c["status"].(string)
		fmt.Printf("    %s region=%-12s %s\n", color.Cyan(fmt.Sprintf("%-14s", id)), region, status)
	}
	fmt.Println(color.Dim("  (tip: pick the legacy \"-1\" cluster for your region — the Terraform provider rejects newer ones)"))
	id := cli.Prompt(in, "OBJ cluster id")
	if id == "" {
		return "", fmt.Errorf("a cluster id is required")
	}
	return id, nil
}

// pushToRepo writes gathered secrets (infra-<env>) + variables (repo-level) into
// instanceRepo. Skips variables whose value already matches the repo. Gated by
// --yes; secret values pipe via stdin.
func pushToRepo(o Opts, repo, env string, secrets, vars map[string]string, st envreq.LiveState) error {
	fmt.Printf("\n%s %s\n", color.Bold("Configure"), repo)
	type item struct {
		argv []string
		val  string
	}
	var items []item
	// Secrets go into infra-<env> unless the requirement table marks them
	// repo-level. Until TF_STATE_ENCRYPTION_PASSPHRASE every instance secret was
	// env-scoped, so this loop hardcoded --env and agreed with the table by
	// coincidence; reading EnvScope makes the table the single source of truth it
	// already claims to be. An unknown name (not in the table) keeps the old
	// env-scoped default.
	for _, k := range cli.SortedKeys(secrets) {
		argv := []string{"gh", "secret", "set", k, "--repo", repo}
		if envreq.SecretIsEnvScoped(k) {
			argv = append(argv, "--env", "infra-"+env)
		}
		items = append(items, item{argv, secrets[k]})
	}
	for _, k := range cli.SortedKeys(vars) {
		if st.Value(k) == vars[k] {
			continue // already set to this value
		}
		items = append(items, item{[]string{"gh", "variable", "set", k, "--repo", repo, "--body", vars[k]}, ""})
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "  (nothing new to push)")
		return nil
	}
	for _, it := range items {
		fmt.Fprintln(os.Stderr, "→ "+ghcli.Quote(it.argv))
	}
	if o.DryRun {
		_ = branchpolicy.Lock(o.DryRun, repo, env) // prints the plan only
		return nil
	}
	if !o.Yes {
		fmt.Fprintln(os.Stderr, "→ lock infra-"+env+" branch policy to main")
		return nil
	}
	// Create + lock the infra-<env> environment BEFORE pushing secrets into it.
	// `gh secret set --env infra-<env>` fetches that environment's public key and
	// 404s if the environment doesn't exist yet; branchpolicy.Lock is what
	// creates it (PUT .../environments/infra-<env>), so it must run first — not
	// after the push loop. It also restricts secret injection to ref=main (the
	// real boundary that stops a feature-branch dispatch from exfiltrating the
	// OpenBao unseal keys).
	protErr := branchpolicy.Lock(o.DryRun, repo, env)
	if protErr != nil && !errors.Is(protErr, branchpolicy.ErrUnsupported) {
		return protErr
	}
	// What llz sent, so the next run can compare EXACTLY instead of ordering file
	// mtimes — see envreq/pushlog.go. Recorded per successful item below.
	sentSecrets := map[string]string{}
	for i, it := range items {
		if err := proc.Run(it.argv, it.val); err != nil {
			// Say WHERE the run stopped and that finishing is a re-run. This loop
			// pushes N secrets/variables and used to abort with a bare
			// `NAME: exit status 1`, leaving the operator with a half-pushed repo and
			// no statement of that fact — in a command whose headline property is
			// that it skips everything already set.
			//lint:ignore ST1005 multi-line operator diagnostic: the trailing period closes an embedded remediation block, not a sentence fragment
			return fmt.Errorf("pushing %s failed (%d of %d pushed): %w\n"+
				"  The items before it ARE set; %s picks up where this stopped\n"+
				"  (it skips everything already satisfied). If this is a permissions\n"+
				"  error, %s reports which credential and scope.",
				it.argv[3], i, len(items), err,
				color.Cyan("llz tokens --env "+env+" --yes"), color.Cyan("llz doctor --env "+env))
		}
		if it.val != "" {
			sentSecrets[it.argv[3]] = it.val
		}
	}
	// Recorded AFTER the loop so a half-pushed run records only what actually
	// landed: the error path above returns, and every item before it is in the
	// map. Best-effort — a log llz cannot write costs accuracy on the next run's
	// drift report (it falls back to mtimes), never this push, which has already
	// succeeded.
	if len(sentSecrets) > 0 {
		if err := envreq.RecordPush(sentSecrets, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", color.Dim(fmt.Sprintf(
				"  (could not record what was pushed in %s: %v — the next drift check falls back to file mtimes)",
				envreq.PushLogFile, err)))
		}
	}
	// The env was created + seeded; if its branch policy couldn't be applied
	// (plan without environment protection), remind the operator at the END.
	if errors.Is(protErr, branchpolicy.ErrUnsupported) {
		branchpolicy.WarnUnsupported(repo, env)
	}
	return nil
}

// configureTemplateHarness sets the template repo's e2e vars + E2E_DISPATCH_TOKEN
// (skipping anything already set).
func configureTemplateHarness(o Opts, in *bufio.Scanner, instanceRepo, clusterID string, st envreq.LiveState) error {
	tr := templateid.Repo()
	fmt.Printf("\n%s e2e harness on %s\n", color.Bold("[admin]"), tr)
	want := map[string]string{
		"E2E_INSTANCE_REPO": instanceRepo,
		"E2E_LINODE_REGION": regionFromCluster(clusterID),
		"E2E_OBJ_CLUSTER":   clusterID,
	}
	var items [][]string
	for _, k := range cli.SortedKeys(want) {
		if want[k] == "" || st.Value(k) == want[k] {
			continue
		}
		items = append(items, []string{"gh", "variable", "set", k, "--repo", tr, "--body", want[k]})
	}
	for _, argv := range items {
		fmt.Fprintln(os.Stderr, "→ "+ghcli.Quote(argv))
	}

	var dispArgv []string
	var dispatch string
	if !st.HasRepoSecret("E2E_DISPATCH_TOKEN") {
		owner := instanceRepo
		if i := strings.IndexByte(instanceRepo, '/'); i > 0 {
			owner = instanceRepo[:i]
		}
		classicURL := ghTokenURL("repo,workflow", "llz-e2e-dispatch")
		fineURL := ghFineGrainedDispatchURL("llz-e2e-dispatch", owner)
		openURL(o, classicURL)
		fmt.Printf("    • E2E_DISPATCH_TOKEN — drives the e2e instance repo %s (force-push the instantiated tree, dispatch/watch its workflows, and open the throwaway PR that proves its PR-gated CI runs)\n", instanceRepo)
		fmt.Printf("      classic (scopes repo + workflow, recommended): %s\n", classicURL)
		fmt.Printf("      fine-grained (then set %s; Only select repositories: %s):\n        %s\n", tokensPromptFineGrained, instanceRepo, fineURL)
		dispatch = cli.Prompt(in, "E2E_DISPATCH_TOKEN (Enter to skip)")
		if dispatch != "" {
			dispArgv = []string{"gh", "secret", "set", "E2E_DISPATCH_TOKEN", "--repo", tr}
			fmt.Fprintln(os.Stderr, "→ "+ghcli.Quote(dispArgv))
		}
	} else {
		fmt.Println(color.Dim("    • E2E_DISPATCH_TOKEN already set — skipping"))
	}

	if o.DryRun || !o.Yes {
		return nil
	}
	for _, argv := range items {
		if err := proc.Run(argv, ""); err != nil {
			return fmt.Errorf("set %s on %s: %w", argv[3], tr, err)
		}
	}
	if dispArgv != nil {
		if err := proc.Run(dispArgv, dispatch); err != nil {
			return fmt.Errorf("set E2E_DISPATCH_TOKEN on %s: %w", tr, err)
		}
	}
	return nil
}
