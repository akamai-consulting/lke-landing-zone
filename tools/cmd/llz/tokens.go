package main

// `llz tokens` provisions the credentials an instance needs to stand up, doing
// the parts the paste-everything wizard (`secrets gather`) can't: it CREATES the
// Terraform-state OBJ bucket + a bucket-scoped key via the Linode API, gathers
// the GitHub PATs (including the Contents:write APL_VALUES_REPO_TOKEN apl-core's
// otomi.git uses), and writes everything to .llz/*.env so it can be pushed.
//
// It is idempotent: it first reads what's already configured (live repo +
// .llz/*.env), prepopulates variable values, prints the readiness plan (the same
// one `llz doctor` shows), and SKIPS anything already configreadiness.Satisfied.
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

// CI image tags published by build-images.yml; TF_IMAGE/KUBE_IMAGE derive from
// these + the template org.
// A THIRD restatement of the image pin, beyond the two the Dockerfile header
// names (build-images.yml's matrix and lint.yml's fallback). It was still on
// 1.9.8 after both of those moved, which would have scaffolded new instances
// onto a HashiCorp Terraform image while every caller invoked `tofu`.
const (
	ciTofuTag       = "1.12.5"
	ciKubernetesTag = "1.31.0"
)

func runTokens(g globalOpts, admin bool, env, cluster, bucket, repo string) error {
	deployEnv := env
	if deployEnv == "" {
		if admin {
			deployEnv = "e2e"
		} else {
			return fmt.Errorf("--env is required (e.g. --env primary)")
		}
	}
	if err := validateEnvName(deployEnv); err != nil {
		return err
	}
	instanceRepo, err := resolveInstanceRepo(repo, admin)
	if err != nil {
		return err
	}
	if err := requireInstanceRepo(instanceRepo); err != nil {
		return err
	}

	fmt.Printf("%s %s\n", color.Bold("llz tokens"), color.Dim(fmt.Sprintf("— %s (env infra-%s)%s", instanceRepo, deployEnv, adminBanner(admin))))

	if err := os.MkdirAll(".llz", 0o700); err != nil {
		return err
	}
	secrets, vars := configreadiness.LoadEnvFiles()

	// Discover existing config (instance + template), pull variable VALUES into
	// vars.env, then print the readiness plan (same as `llz doctor`).
	reqs := configreadiness.E2ERequirements(admin)
	instSt := configreadiness.FetchLiveState(instanceRepo, deployEnv)
	var tmplSt configreadiness.LiveState
	if admin {
		tmplSt = configreadiness.FetchLiveState(templateRepo(), "")
	}
	if n := configreadiness.PrepopulateVars(vars, reqs, instSt, tmplSt); n > 0 {
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
	// A fresh instance costs nothing here — staleCIImageVars returns before its
	// network round-trips when neither variable is recorded yet.
	repin := staleCIImageVars(pinnedTemplateRef(), func(k string) string {
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
	validity, invalidN := probeTokenValidities(reqs, secrets, vars, instSt, ghcrUser)
	missing := configreadiness.ReportReadiness(reqs, secrets, vars, instSt, tmplSt, validity)
	if invalidN > 0 {
		fmt.Println(color.Dim("  (fix the invalid credential(s) above, then re-run — a dead token fails the CI run later)"))
	}
	if len(missing) == 0 && len(repin) == 0 {
		_ = writeEnvFile(".llz/vars.env", vars)
		fmt.Printf("\n%s Everything required for e2e is already set — nothing to do.\n", color.Green("✓"))
		return nil
	}
	if g.dryRun {
		fmt.Printf("\n%s\n", color.Dim(fmt.Sprintf("(dry-run) would provision the %d missing REQUIRED item(s) above%s.",
			len(missing), repinPlanNote(repin))))
		return nil
	}
	if !g.yes {
		fmt.Println("\n" + color.Dim("(no --yes: will gather + write .llz/*.env + print the push plan, but create/write nothing)"))
	}

	// Decide the state-encryption passphrase's fate BEFORE the interactive section
	// below: its only failure mode is a hard refusal, and refusing after the
	// prompts would throw away three freshly-pasted PATs and orphan a
	// bucket-scoped OBJ key this run created. See state_passphrase.go.
	passPlan, err := planStatePassphrase(instanceRepo, deployEnv, secrets)
	if err != nil {
		return err
	}

	// have(name) — already configreadiness.Satisfied (env file or live instance repo) → skip.
	have := func(name string, secret bool) bool {
		return configreadiness.Satisfied(configreadiness.Requirement{Name: name, Secret: secret}, secrets, vars, instSt)
	}
	in := bufio.NewScanner(os.Stdin)

	// ── Linode: PAT + state bucket + scoped key ──────────────────────────────
	needKeys := !have("TF_STATE_ACCESS_KEY", true) || !have("TF_STATE_SECRET_KEY", true)
	clusterID := clusterFromEndpoint(vars["TF_STATE_ENDPOINT"])
	if needKeys || !have("LINODE_API_TOKEN", true) {
		fmt.Printf("\n%s API token — full Read/Write (provisioning; also creates the state bucket)\n", color.Bold("[Linode]"))
		openURL(g, linodeTokensURL)
		fmt.Printf("      %s %s\n", color.Dim("create at:"), color.Cyan(linodeTokensURL))
		token := prompt(in, "Linode PAT")
		if token == "" {
			return fmt.Errorf("a Linode PAT is required")
		}
		if !have("LINODE_API_TOKEN", true) {
			secrets["LINODE_API_TOKEN"] = token
		}
		if needKeys {
			client := linode.NewClient(token, 30*time.Second)
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
			if g.yes {
				if _, err := client.CreateObjectStorageBucket(ctx, clusterID, bucketName); err != nil {
					return fmt.Errorf("create bucket: %w", err)
				}
				key, err := client.CreateObjectStorageKey(ctx, "llz-tfstate-"+repoSlug(instanceRepo), clusterID, bucketName, "read_write")
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
		openURL(g, primaryURL)
		fmt.Printf("\n%s %s — %s\n", color.Bold("[GitHub]"), name, note)
		fmt.Printf("      %s:\n        %s\n", primaryLabel, color.Cyan(primaryURL))
		if altURL != "" {
			fmt.Printf("      %s:\n        %s\n", color.Dim(altLabel), color.Cyan(altURL))
		}
		if v := prompt(in, name); v != "" {
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
	// configreadiness.Satisfied" — so doing that work for two variables it is not going to touch
	// both slows the idempotent path and, worse, warns that TF_IMAGE/KUBE_IMAGE are
	// unpinned when they are already set to something the operator chose.
	needTF, needKube := !have("TF_IMAGE", false), !have("KUBE_IMAGE", false)
	if needTF || needKube {
		computeAndReportImageVars(vars, needTF, needKube)
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
		if v := prompt(in, s.name+" (Enter to skip)"); v != "" {
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
		if v := prompt(in, "GHCR_READ_TOKEN (Enter to skip)"); v != "" {
			secrets["GHCR_READ_TOKEN"] = v
			if !have("GHCR_USERNAME", false) {
				if u := prompt(in, "GHCR_USERNAME (owner of that PAT)"); u != "" {
					vars["GHCR_USERNAME"] = u
				}
			}
		}
	}

	// ── state-encryption passphrase ──────────────────────────────────────────
	// Generated, not prompted: it is machine material with no issuer to visit, and
	// every Terraform root refuses to run without it.
	if err := ensureStatePassphrase(g, passPlan, instanceRepo, secrets); err != nil {
		return err
	}

	// ── persist + push ───────────────────────────────────────────────────────
	if err := writeEnvFile(".llz/secrets.env", secrets); err != nil {
		return err
	}
	if err := writeEnvFile(".llz/vars.env", vars); err != nil {
		return err
	}
	// AFTER the cache is written, so the operator keeps their local copy, and
	// BEFORE the push, which re-sets every secret in the map unconditionally. The
	// repo's passphrase is authoritative once it exists: pushing a cached one over
	// it is a no-op at best and, against a rotated repo, strands every state file.
	// Re-asked here rather than trusting passPlan, because the plan was made before
	// the interactive section and the repo can have acquired a passphrase since.
	if err := dropStatePassphraseIfLive(instanceRepo, deployEnv, secrets, passPlan.generate); err != nil {
		return err
	}
	nSecrets, nVars := len(secrets), len(vars)
	fmt.Printf("\n%s wrote %d secret(s) + %d variable(s) to .llz/\n", color.Green("✓"), nSecrets, nVars)

	// Admin e2e harness (template-repo vars + E2E_DISPATCH_TOKEN) runs BEFORE the
	// instance-repo push: a push / branch-policy failure on the instance repo
	// must not suppress the E2E_DISPATCH_TOKEN creation link the maintainer needs.
	if admin {
		if err := configureTemplateHarness(g, in, instanceRepo, clusterID, tmplSt); err != nil {
			return err
		}
	}
	if err := pushToRepo(g, instanceRepo, deployEnv, secrets, vars, instSt); err != nil {
		return err
	}
	if !g.yes {
		fmt.Println("\n" + color.Dim("(no --yes: nothing was created or pushed — re-run with --yes to execute)"))
	}
	return nil
}

// printTokensNextSteps prints the recommended flow after a real `llz tokens` run
// (credentials provisioned + pushed). Only the standalone command calls it — the
// `llz up` chain runs tokens → doctor → build itself and prints its own guidance.
func printTokensNextSteps(env string) {
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

// cmdDoctorE2E reports e2e readiness of the env files + live repo (the wizard's
// plan, runnable standalone). Wired as `llz doctor` (see cmdDoctor).
func cmdDoctorE2E(repo, env string, admin bool) error {
	instanceRepo, err := resolveInstanceRepo(repo, admin)
	if err != nil {
		return err
	}
	if env == "" {
		env = "e2e"
	}
	if err := requireInstanceRepo(instanceRepo); err != nil {
		return err
	}
	secrets, vars := configreadiness.LoadEnvFiles()
	reqs := configreadiness.E2ERequirements(admin)
	instSt := configreadiness.FetchLiveState(instanceRepo, env)
	var tmplSt configreadiness.LiveState
	if admin {
		tmplSt = configreadiness.FetchLiveState(templateRepo(), "")
	}
	fmt.Printf("\n%s\n", color.Bold(fmt.Sprintf("e2e readiness — %s (infra-%s)%s", instanceRepo, env, adminBanner(admin))))
	// Actively probe validity, not just presence — a set-but-dead token is the
	// failure that otherwise only shows up as a 401/403 mid-CI-run.
	ghcrUser := firstNonEmpty(vars["GHCR_USERNAME"], instSt.Value("GHCR_USERNAME"))
	validity, invalid := probeTokenValidities(reqs, secrets, vars, instSt, ghcrUser)
	missing := configreadiness.ReportReadiness(reqs, secrets, vars, instSt, tmplSt, validity)
	if len(missing) > 0 {
		fmt.Printf("\n%s %d required item(s) missing: %s\n", color.Red("✗"), len(missing), strings.Join(missing, ", "))
		fmt.Println("  run `llz tokens" + adminFlag(admin) + " --env " + env + " --yes` to provision them.")
	}
	if invalid > 0 {
		return fmt.Errorf("%d probeable credential(s) are invalid — rotate them (see the validity report above)", invalid)
	}
	if len(missing) == 0 {
		fmt.Println("\n" + color.Green("✓") + " ready — every required value is set and every probeable token is valid.")
	}
	return nil
}

func adminFlag(admin bool) string {
	if admin {
		return " --admin"
	}
	return ""
}

// ── helpers ──────────────────────────────────────────────────────────────────

func resolveInstanceRepo(flagVal string, admin bool) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if a, _ := readAnswers("."); a != nil && a.InstanceRepo != "" {
		return a.InstanceRepo, nil
	}
	if admin {
		return defaultTemplateOrg + "/" + templateName + "-example", nil
	}
	return "", fmt.Errorf("could not determine instance repo — pass --repo <owner>/<name>")
}

// repoExists reports whether the GitHub repo is reachable (it exists and the
// authenticated token can see it). The usual cause of a doctor/tokens run where
// every secret reads "missing" is that the natural `llz new` (without --push) →
// `llz tokens` sequence never created the remote repo.
func repoExists(repo string) bool {
	found, err := repoStatus(repo)
	return found && err == nil
}

// repoStatus is repoExists with the third answer kept: (false, nil) means GitHub
// answered 404, while a non-nil error means llz could not ask at all — `gh`
// absent, unauthenticated, offline, rate-limited. Collapsing those two into
// "false" is fine for a readiness table (every row reads "missing" either way)
// but wrong for a preflight that then blames the repo for gh's problem.
//
// A 404 is "not there, OR not visible to this login" — GitHub hides private
// repos behind the same status rather than admitting they exist. Callers must
// word it that way: an operator authed as the wrong account, or with a token
// missing the repo scope, is told to create a repository that is already there,
// and `gh repo create` then dead-ends on "Name already exists on this account".
func repoStatus(repo string) (bool, error) {
	if _, err := execLookPath("gh"); err != nil {
		return false, fmt.Errorf("the GitHub CLI is not on PATH: %w", err)
	}
	if _, err := execOutput("gh", "api", "repos/"+repo, "--silent"); err != nil {
		if ghNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// requireInstanceRepo gates `llz tokens` / `llz doctor` on the live instance repo
// — both read and write it, so neither means anything without it. It keeps the
// two failures apart for the same reason `llz new`'s preflight does: a `gh` that
// cannot answer (missing, unauthenticated, offline, rate-limited) used to be
// reported as "instance repo not found on GitHub", sending the operator off to
// re-create a repo that was never missing. Skipped entirely when gh is absent —
// doctor's own tooling table is where that belongs, and `llz tokens` fails on it
// soon enough.
func requireInstanceRepo(instanceRepo string) error {
	if !lookable("gh") {
		return nil
	}
	found, err := repoStatus(instanceRepo)
	switch {
	case err != nil:
		return ghUnreachableErr(instanceRepo, err,
			"This does NOT mean the repo is missing — nothing was checked. Re-run once gh can answer")
	case !found:
		remediateMissingRepo(instanceRepo)
		return fmt.Errorf("instance repo %s is not visible to your `gh` login (absent, or private to an account you are not authed as)", instanceRepo)
	}
	return nil
}

// remediateMissingRepo prints the exact fix for an absent instance repo so the
// failure is actionable instead of an all-missing readiness table.
func remediateMissingRepo(repo string) {
	fmt.Fprintf(os.Stderr, "\n%s instance repo %q is not reachable on GitHub.\n", color.Red("✗"), repo)
	fmt.Fprintln(os.Stderr, "  `llz tokens` and `llz doctor` read/write the live repo, so it must exist and be pushed first.")
	// GitHub 404s a private repo you cannot see exactly as it 404s one that does
	// not exist, so "create it" is the wrong first move for an operator who is
	// simply authed as the wrong account — `gh repo create` would then dead-end
	// on "Name already exists on this account".
	fmt.Fprintf(os.Stderr, "  If it DOES exist, you are authed as an account that cannot see it: gh auth status --hostname %s\n", ghHost())
	// An absent OWNER is the more common cause and needs a different first step —
	// `gh repo create` makes a repository, never the org that holds it, so
	// printing the create line alone sends the operator into a bare
	// "does not have the correct permissions to execute `CreateRepository`".
	// Spelling comes first: a user owner they can log in as always exists, so an
	// absent one is far more often a typo in instance_repo than an uncreated org.
	if owner, _, ok := strings.Cut(repo, "/"); ok {
		if kind, err := ghOwnerKindFn(owner); err == nil && kind == "" {
			fmt.Fprintf(os.Stderr, "  The OWNER %q does not exist either — check how it is spelled in .copier-answers.yml,\n", owner)
			fmt.Fprintf(os.Stderr, "  or, if that org is simply not created yet: https://%s/organizations/new\n", ghHost())
		}
	}
	// `--source . --push` pushes whatever branch is checked out, and `git init`
	// still names the first one `master` unless init.defaultBranch says otherwise.
	// `llz new --push` normalises that itself (ensureScaffoldBranch); a remediation
	// the operator types by hand in some existing checkout does not, and a repo
	// whose content lands on `master` leaves the platform-bootstrap Application
	// asking Argo CD for a revision that is not there.
	fmt.Fprintln(os.Stderr, "  Create + push it from the instance directory — on `main`, which the platform-bootstrap")
	fmt.Fprintln(os.Stderr, "  Application tracks (apps_repo_revision); `git branch -M main` first if you are on master:")
	fmt.Fprintf(os.Stderr, "    gh repo create %s --private --source . --remote origin --push\n", repo)
	fmt.Fprintln(os.Stderr, "  …or re-scaffold with push next time: `llz new <name> --push --yes`.")
}

func templateRepo() string { return defaultTemplateOrg + "/" + templateName }

func adminBanner(admin bool) string {
	if admin {
		return " [ADMIN: + " + templateRepo() + " e2e harness]"
	}
	return ""
}

func repoSlug(repo string) string {
	if _, name, ok := strings.Cut(repo, "/"); ok {
		return strings.ToLower(name)
	}
	return strings.ToLower(repo)
}

// repinPlanNote is the dry-run tail that keeps a repin-only run from reporting
// "0 missing REQUIRED item(s)" and nothing else — which reads as "no work" for
// precisely the run that has some.
func repinPlanNote(repin []ciImageSkew) string {
	if len(repin) == 0 {
		return ""
	}
	return fmt.Sprintf(" and re-pin %d ci image variable(s) to the current template pin", len(repin))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

func prompt(in *bufio.Scanner, label string) string {
	fmt.Printf("  %s: ", label)
	if !in.Scan() {
		return ""
	}
	return strings.TrimSpace(in.Text())
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
	id := prompt(in, "OBJ cluster id")
	if id == "" {
		return "", fmt.Errorf("a cluster id is required")
	}
	return id, nil
}

// pushToRepo writes gathered secrets (infra-<env>) + variables (repo-level) into
// instanceRepo. Skips variables whose value already matches the repo. Gated by
// --yes; secret values pipe via stdin.
func pushToRepo(g globalOpts, repo, env string, secrets, vars map[string]string, st configreadiness.LiveState) error {
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
	for _, k := range sortedKeys(secrets) {
		argv := []string{"gh", "secret", "set", k, "--repo", repo}
		if configreadiness.SecretIsEnvScoped(k) {
			argv = append(argv, "--env", "infra-"+env)
		}
		items = append(items, item{argv, secrets[k]})
	}
	for _, k := range sortedKeys(vars) {
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
		fmt.Fprintln(os.Stderr, "→ "+shellQuote(it.argv))
	}
	if g.dryRun {
		_ = lockInfraEnvBranchPolicy(g, repo, env) // prints the plan only
		return nil
	}
	if !g.yes {
		fmt.Fprintln(os.Stderr, "→ lock infra-"+env+" branch policy to main")
		return nil
	}
	// Create + lock the infra-<env> environment BEFORE pushing secrets into it.
	// `gh secret set --env infra-<env>` fetches that environment's public key and
	// 404s if the environment doesn't exist yet; lockInfraEnvBranchPolicy is what
	// creates it (PUT .../environments/infra-<env>), so it must run first — not
	// after the push loop. It also restricts secret injection to ref=main (the
	// real boundary that stops a feature-branch dispatch from exfiltrating the
	// OpenBao unseal keys).
	protErr := lockInfraEnvBranchPolicy(g, repo, env)
	if protErr != nil && !errors.Is(protErr, errEnvProtectionUnsupported) {
		return protErr
	}
	for _, it := range items {
		if err := execArgv(it.argv, it.val); err != nil {
			return fmt.Errorf("%s: %w", it.argv[3], err)
		}
	}
	// The env was created + seeded; if its branch policy couldn't be applied
	// (plan without environment protection), remind the operator at the END.
	if errors.Is(protErr, errEnvProtectionUnsupported) {
		warnEnvProtectionUnsupported(repo, env)
	}
	return nil
}

// configureTemplateHarness sets the template repo's e2e vars + E2E_DISPATCH_TOKEN
// (skipping anything already set).
func configureTemplateHarness(g globalOpts, in *bufio.Scanner, instanceRepo, clusterID string, st configreadiness.LiveState) error {
	tr := templateRepo()
	fmt.Printf("\n%s e2e harness on %s\n", color.Bold("[admin]"), tr)
	want := map[string]string{
		"E2E_INSTANCE_REPO": instanceRepo,
		"E2E_LINODE_REGION": regionFromCluster(clusterID),
		"E2E_OBJ_CLUSTER":   clusterID,
	}
	var items [][]string
	for _, k := range sortedKeys(want) {
		if want[k] == "" || st.Value(k) == want[k] {
			continue
		}
		items = append(items, []string{"gh", "variable", "set", k, "--repo", tr, "--body", want[k]})
	}
	for _, argv := range items {
		fmt.Fprintln(os.Stderr, "→ "+shellQuote(argv))
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
		openURL(g, classicURL)
		fmt.Printf("    • E2E_DISPATCH_TOKEN — drives the e2e instance repo %s (force-push the instantiated tree + dispatch/watch its workflows)\n", instanceRepo)
		fmt.Printf("      classic (scopes repo + workflow, recommended): %s\n", classicURL)
		fmt.Printf("      fine-grained (then set Contents + Actions + Workflows: Read and write; Only select repositories: %s):\n        %s\n", instanceRepo, fineURL)
		dispatch = prompt(in, "E2E_DISPATCH_TOKEN (Enter to skip)")
		if dispatch != "" {
			dispArgv = []string{"gh", "secret", "set", "E2E_DISPATCH_TOKEN", "--repo", tr}
			fmt.Fprintln(os.Stderr, "→ "+shellQuote(dispArgv))
		}
	} else {
		fmt.Println(color.Dim("    • E2E_DISPATCH_TOKEN already set — skipping"))
	}

	if g.dryRun || !g.yes {
		return nil
	}
	for _, argv := range items {
		if err := execArgv(argv, ""); err != nil {
			return fmt.Errorf("set %s on %s: %w", argv[3], tr, err)
		}
	}
	if dispArgv != nil {
		if err := execArgv(dispArgv, dispatch); err != nil {
			return fmt.Errorf("set E2E_DISPATCH_TOKEN on %s: %w", tr, err)
		}
	}
	return nil
}
