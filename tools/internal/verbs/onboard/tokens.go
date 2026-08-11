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
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghapi"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
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
	missing := envreq.ReportReadiness(reqs, secrets, vars, instSt, tmplSt, validity)
	if invalidN > 0 {
		fmt.Println(color.Dim("  (fix the invalid credential(s) above, then re-run — a dead token fails the CI run later)"))
	}
	if len(missing) == 0 && len(repin) == 0 {
		_ = WriteEnvFile(".llz/vars.env", vars)
		fmt.Printf("\n%s Everything required for e2e is already set — nothing to do.\n", color.Green("✓"))
		return nil
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
	// OPENBAO_SECRETS_WRITE_TOKEN: CI's `gh secret set` persists the OpenBao
	// unseal keys back into the infra-<env> environment. Fine-grained needs
	// Actions: write + ENVIRONMENTS: write — not "Secrets", which governs only
	// repo-level secrets and leaves environment writes 403ing (see
	// ghFineGrainedSecretsWriteURL). A classic repo+workflow PAT works too —
	// offer both. Either way the PAT owner must be Environment admin on every
	// infra-<env> environment, or the --env-scoped writes 401.
	gatherGH("OPENBAO_SECRETS_WRITE_TOKEN",
		"CI persists OpenBao unseal keys into the infra-<env> environment (you must also be Environment admin on it)",
		"fine-grained, recommended (Actions: write + Environments: write; Only select repositories: "+instanceRepo+")",
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
	fmt.Println(color.Dim("  after the first build: escrow OpenBao unseal keys 4 & 5 + the root token offline,"))
	fmt.Println(color.Dim("  and delete OPENBAO_ROOT_TOKEN from infra-" + env + " (DNS-01 certs wire automatically via TF_VAR_linode_dns_token)."))
}

// DoctorE2E reports e2e readiness of the env files + live repo (the wizard's
// plan, runnable standalone). Wired as `llz doctor` (see cmdDoctor).
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
	missing := envreq.ReportReadiness(reqs, secrets, vars, instSt, tmplSt, validity)
	// PRESENCE is not FRESHNESS. reportReadiness ticks TF_IMAGE/KUBE_IMAGE as set;
	// `llz ci assert-image-fresh` — the first step of the apply's first job —
	// additionally requires them to name THIS instance's pin. Same merged lookup
	// `llz tokens` re-pins from, so doctor sees what CI will see.
	pinErr := checkCIImagePins(func(k string) string {
		return firstNonEmpty(vars[k], instSt.Value(k))
	})
	if len(missing) > 0 {
		fmt.Printf("\n%s %d required item(s) missing: %s\n", color.Red("✗"), len(missing), strings.Join(missing, ", "))
		fmt.Println("  run `llz tokens" + adminFlag(admin) + " --env " + env + " --yes` to provision them.")
	}
	if invalid > 0 {
		return fmt.Errorf("%d probeable credential(s) are invalid — rotate them (see the validity report above)", invalid)
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
	fmt.Println("\n" + color.Green("✓") + " ready — every required value is set and every probeable token is valid.")
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
func clusterFromEndpoint(endpoint string) string {
	s := strings.TrimPrefix(endpoint, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, ".linodeobjects.com"); i > 0 {
		return s[:i]
	}
	return ""
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
