package main

// `llz tokens` provisions the credentials an instance needs to stand up, doing
// the parts the paste-everything wizard (`secrets gather`) can't: it CREATES the
// Terraform-state OBJ bucket + a bucket-scoped key via the Linode API, gathers
// the GitHub PATs (including the Contents:write APL_VALUES_REPO_TOKEN apl-core's
// otomi.git uses), and writes everything to .llz/*.env so it can be pushed.
//
// It is idempotent: it first reads what's already configured (live repo +
// .llz/*.env), prepopulates variable values, prints the readiness plan (the same
// one `llz doctor` shows), and SKIPS anything already satisfied.
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// CI image tags published by build-images.yml; TF_IMAGE/KUBE_IMAGE derive from
// these + the template org.
const (
	ciTerraformTag  = "1.9.8"
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
	if lookable("gh") && !repoExists(instanceRepo) {
		remediateMissingRepo(instanceRepo)
		return fmt.Errorf("instance repo %s not found on GitHub", instanceRepo)
	}

	fmt.Printf("%s %s\n", bold("llz tokens"), dim(fmt.Sprintf("— %s (env infra-%s)%s", instanceRepo, deployEnv, adminBanner(admin))))

	if err := os.MkdirAll(".llz", 0o700); err != nil {
		return err
	}
	secrets, vars := loadEnvFiles()

	// Discover existing config (instance + template), pull variable VALUES into
	// vars.env, then print the readiness plan (same as `llz doctor`).
	reqs := e2eRequirements(admin)
	instSt := fetchLiveState(instanceRepo, deployEnv)
	var tmplSt liveState
	if admin {
		tmplSt = fetchLiveState(templateRepo(), "")
	}
	if n := prepopulateVars(vars, reqs, instSt, tmplSt); n > 0 {
		fmt.Printf("%s\n", dim(fmt.Sprintf("Prepopulated %d variable value(s) from existing repo config.", n)))
	}
	missing := reportReadiness(reqs, secrets, vars, instSt, tmplSt)
	if len(missing) == 0 {
		_ = writeEnvFile(".llz/vars.env", vars)
		fmt.Printf("\n%s Everything required for e2e is already set — nothing to do.\n", green("✓"))
		return nil
	}
	if g.dryRun {
		fmt.Printf("\n%s\n", dim(fmt.Sprintf("(dry-run) would provision the %d missing REQUIRED item(s) above.", len(missing))))
		return nil
	}
	if !g.yes {
		fmt.Println("\n" + dim("(no --yes: will gather + write .llz/*.env + print the push plan, but create/write nothing)"))
	}

	// have(name) — already satisfied (env file or live instance repo) → skip.
	have := func(name string, secret bool) bool {
		return satisfied(requirement{Name: name, Secret: secret}, secrets, vars, instSt)
	}
	in := bufio.NewScanner(os.Stdin)

	// ── Linode: PAT + state bucket + scoped key ──────────────────────────────
	needKeys := !have("TF_STATE_ACCESS_KEY", true) || !have("TF_STATE_SECRET_KEY", true)
	clusterID := clusterFromEndpoint(vars["TF_STATE_ENDPOINT"])
	if needKeys || !have("LINODE_API_TOKEN", true) {
		fmt.Printf("\n%s API token — full Read/Write (provisioning; also creates the state bucket)\n", bold("[Linode]"))
		openURL(g, linodeTokensURL)
		fmt.Printf("      %s %s\n", dim("create at:"), cyan(linodeTokensURL))
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
			fmt.Printf("%s state bucket %q in %s\n", bold("[Linode]"), bucketName, clusterID)
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
				fmt.Printf("      %s bucket + scoped read_write key created\n", green("✓"))
			} else {
				fmt.Println(dim("      (--yes to create the bucket + scoped key)"))
			}
		}
	} else {
		fmt.Println("\n" + bold("[Linode]") + dim(" token + state bucket/key already set — skipping"))
	}

	// ── GitHub PATs ──────────────────────────────────────────────────────────
	// gatherGH prompts for one PAT. It opens + prints the primary minting link;
	// when altURL is non-empty it also prints an alternate option (e.g. classic
	// vs fine-grained) so the operator can pick whichever their org policy allows.
	gatherGH := func(name, note, primaryLabel, primaryURL, altLabel, altURL string) {
		if have(name, true) {
			fmt.Printf("%s %s\n", bold("[GitHub]"), dim(name+" already set — skipping"))
			return
		}
		openURL(g, primaryURL)
		fmt.Printf("\n%s %s — %s\n", bold("[GitHub]"), name, note)
		fmt.Printf("      %s:\n        %s\n", primaryLabel, cyan(primaryURL))
		if altURL != "" {
			fmt.Printf("      %s:\n        %s\n", dim(altLabel), cyan(altURL))
		}
		if v := prompt(in, name); v != "" {
			secrets[name] = v
		}
	}
	owner, _, _ := strings.Cut(instanceRepo, "/")
	// OPENBAO_SECRETS_WRITE_TOKEN: CI's `gh secret set` persists the OpenBao
	// unseal keys back into the infra-<env> environment. The
	// consuming workflow (llz-bootstrap-openbao.yml) documents fine-grained
	// Actions + Secrets: write, but a classic repo+workflow PAT works too — offer
	// both. Either way the PAT owner must be Environment admin on every
	// infra-<env> environment, or the --env-scoped writes 401.
	gatherGH("OPENBAO_SECRETS_WRITE_TOKEN",
		"CI persists OpenBao unseal keys into the infra-<env> environment (you must also be Environment admin on it)",
		"fine-grained, recommended (Actions + Secrets: write; Only select repositories: "+instanceRepo+")",
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
	// ArgoCD pulls them anonymously — no GHCR_READ_TOKEN is provisioned here. A
	// private fork or the optional Akamai-internal firewall-controller image can
	// still set GHCR_READ_TOKEN + GHCR_USERNAME by hand; the TF gate honors it.)

	// ── Computed vars ────────────────────────────────────────────────────────
	if !have("TF_IMAGE", false) {
		vars["TF_IMAGE"] = fmt.Sprintf("ghcr.io/%s/ci-terraform:%s", strings.ToLower(defaultTemplateOrg), ciTerraformTag)
	}
	if !have("KUBE_IMAGE", false) {
		vars["KUBE_IMAGE"] = fmt.Sprintf("ghcr.io/%s/ci-kubernetes:%s", strings.ToLower(defaultTemplateOrg), ciKubernetesTag)
	}

	// ── Optional secrets ─────────────────────────────────────────────────────
	for _, s := range []struct{ name, desc string }{
		{"LINODE_DNS_TOKEN", "Linode token, Domains: Read/Write (cert-manager DNS-01)"},
		{"CLOUD_FIREWALL_TOKEN", "Linode token scoped to Cloud Firewalls"},
	} {
		if have(s.name, true) {
			continue
		}
		fmt.Printf("\n%s %s — %s\n", bold("[optional]"), s.name, dim(s.desc))
		if v := prompt(in, s.name+" (Enter to skip)"); v != "" {
			secrets[s.name] = v
		}
	}

	// ── persist + push ───────────────────────────────────────────────────────
	if err := writeEnvFile(".llz/secrets.env", secrets); err != nil {
		return err
	}
	if err := writeEnvFile(".llz/vars.env", vars); err != nil {
		return err
	}
	fmt.Printf("\n%s wrote %d secret(s) + %d variable(s) to .llz/\n", green("✓"), len(secrets), len(vars))

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
		fmt.Println("\n" + dim("(no --yes: nothing was created or pushed — re-run with --yes to execute)"))
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
		fmt.Printf("  %s%s%s\n", cyan(c), strings.Repeat(" ", pad), dim("# "+note))
	}
	fmt.Println("\n" + bold("Next steps"))
	cmd("llz doctor --env "+env, "confirm every required value is set")
	cmd("llz build "+env+" --yes", "dispatch the apply  (or `llz up "+env+" --yes` chains doctor → build)")
	cmd("llz status "+env, "watch OpenBao / ArgoCD / ESO converge")
	fmt.Println(dim("  after the first build: escrow OpenBao unseal keys 4 & 5 + the root token offline,"))
	fmt.Println(dim("  delete OPENBAO_ROOT_TOKEN from infra-" + env + ", then `llz bootstrap dns " + env + " --yes`."))
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
	if lookable("gh") && !repoExists(instanceRepo) {
		remediateMissingRepo(instanceRepo)
		return fmt.Errorf("instance repo %s not found on GitHub", instanceRepo)
	}
	secrets, vars := loadEnvFiles()
	reqs := e2eRequirements(admin)
	instSt := fetchLiveState(instanceRepo, env)
	var tmplSt liveState
	if admin {
		tmplSt = fetchLiveState(templateRepo(), "")
	}
	fmt.Printf("\n%s\n", bold(fmt.Sprintf("e2e readiness — %s (infra-%s)%s", instanceRepo, env, adminBanner(admin))))
	missing := reportReadiness(reqs, secrets, vars, instSt, tmplSt)
	if len(missing) == 0 {
		fmt.Println("\n" + green("✓") + " ready — every required value is set.")
		return nil
	}
	fmt.Printf("\n%s %d required item(s) missing: %s\n", red("✗"), len(missing), strings.Join(missing, ", "))
	fmt.Println("  run `llz tokens" + adminFlag(admin) + " --env " + env + " --yes` to provision them.")
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
	_, err := execOutput("gh", "api", "repos/"+repo, "--silent")
	return err == nil
}

// remediateMissingRepo prints the exact fix for an absent instance repo so the
// failure is actionable instead of an all-missing readiness table.
func remediateMissingRepo(repo string) {
	fmt.Fprintf(os.Stderr, "\n%s instance repo %q is not reachable on GitHub.\n", red("✗"), repo)
	fmt.Fprintln(os.Stderr, "  `llz tokens` and `llz doctor` read/write the live repo, so it must exist and be pushed first.")
	fmt.Fprintln(os.Stderr, "  Create + push it from the instance directory:")
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
	fmt.Println("\n  " + bold("Object Storage clusters:"))
	for _, c := range clusters {
		id, _ := c["id"].(string)
		region, _ := c["region"].(string)
		status, _ := c["status"].(string)
		fmt.Printf("    %s region=%-12s %s\n", cyan(fmt.Sprintf("%-14s", id)), region, status)
	}
	fmt.Println(dim("  (tip: pick the legacy \"-1\" cluster for your region — the Terraform provider rejects newer ones)"))
	id := prompt(in, "OBJ cluster id")
	if id == "" {
		return "", fmt.Errorf("a cluster id is required")
	}
	return id, nil
}

// pushToRepo writes gathered secrets (infra-<env>) + variables (repo-level) into
// instanceRepo. Skips variables whose value already matches the repo. Gated by
// --yes; secret values pipe via stdin.
func pushToRepo(g globalOpts, repo, env string, secrets, vars map[string]string, st liveState) error {
	fmt.Printf("\n%s %s\n", bold("Configure"), repo)
	type item struct {
		argv []string
		val  string
	}
	var items []item
	for _, k := range sortedKeys(secrets) {
		items = append(items, item{[]string{"gh", "secret", "set", k, "--repo", repo, "--env", "infra-" + env}, secrets[k]})
	}
	for _, k := range sortedKeys(vars) {
		if st.value(k) == vars[k] {
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
func configureTemplateHarness(g globalOpts, in *bufio.Scanner, instanceRepo, clusterID string, st liveState) error {
	tr := templateRepo()
	fmt.Printf("\n%s e2e harness on %s\n", bold("[admin]"), tr)
	want := map[string]string{
		"E2E_INSTANCE_REPO": instanceRepo,
		"E2E_LINODE_REGION": regionFromCluster(clusterID),
		"E2E_OBJ_CLUSTER":   clusterID,
	}
	var items [][]string
	for _, k := range sortedKeys(want) {
		if want[k] == "" || st.value(k) == want[k] {
			continue
		}
		items = append(items, []string{"gh", "variable", "set", k, "--repo", tr, "--body", want[k]})
	}
	for _, argv := range items {
		fmt.Fprintln(os.Stderr, "→ "+shellQuote(argv))
	}

	var dispArgv []string
	var dispatch string
	if !st.repoSecrets["E2E_DISPATCH_TOKEN"] {
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
		fmt.Println(dim("    • E2E_DISPATCH_TOKEN already set — skipping"))
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
