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

	fmt.Printf("llz tokens — %s (env infra-%s)%s\n", instanceRepo, deployEnv, adminBanner(admin))

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
		fmt.Printf("Prepopulated %d variable value(s) from existing repo config.\n", n)
	}
	missing := reportReadiness(reqs, secrets, vars, instSt, tmplSt)
	if len(missing) == 0 {
		_ = writeEnvFile(".llz/vars.env", vars)
		fmt.Println("\nEverything required for e2e is already set — nothing to do.")
		return nil
	}
	if g.dryRun {
		fmt.Printf("\n(dry-run) would provision the %d missing REQUIRED item(s) above.\n", len(missing))
		return nil
	}
	if !g.yes {
		fmt.Println("\n(no --yes: will gather + write .llz/*.env + print the push plan, but create/write nothing)")
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
		fmt.Println("\n[Linode] API token — full Read/Write (provisioning; also creates the state bucket)")
		openURL(g, linodeTokensURL)
		fmt.Printf("      create at: %s\n", linodeTokensURL)
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
			fmt.Printf("[Linode] state bucket %q in %s\n", bucketName, clusterID)
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
				fmt.Println("      ✓ bucket + scoped read_write key created")
			} else {
				fmt.Println("      (--yes to create the bucket + scoped key)")
			}
		}
	} else {
		fmt.Println("\n[Linode] token + state bucket/key already set — skipping")
	}

	// ── GitHub PATs ──────────────────────────────────────────────────────────
	gatherGH := func(name, url, note string) {
		if have(name, true) {
			fmt.Printf("[GitHub] %s already set — skipping\n", name)
			return
		}
		openURL(g, url)
		fmt.Printf("\n[GitHub] %s — %s\n      %s\n", name, note, url)
		if v := prompt(in, name); v != "" {
			secrets[name] = v
		}
	}
	gatherGH("OPENBAO_SECRETS_WRITE_TOKEN", ghTokenURL("repo,workflow", "llz-openbao-secrets-write"), "classic PAT, scopes repo+workflow")
	// HARD-required by terraform apply: apl-core's otomi.git.password + the
	// argocd repo Secrets. apl-operator PUSHES its values tree to this repo, so
	// the PAT needs Contents: write (the in-cluster Gitea is obsoleted). The
	// template URL pre-fills name/owner/Contents:write; GitHub can't pre-select
	// the specific repo, so the note tells the operator to pick it.
	aplOwner, _, _ := strings.Cut(instanceRepo, "/")
	gatherGH("APL_VALUES_REPO_TOKEN", ghFineGrainedTokenURL("llz-apl-values-repo", aplOwner, "apl-core values repo (otomi.git) + argocd repo Secrets"), "fine-grained PAT (Contents: write pre-filled) → Only select repositories: "+instanceRepo)
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
		{"LOKI_ADMIN_PASSWORD", "Loki gateway basic-auth password"},
		{"CLOUD_FIREWALL_TOKEN", "Linode token scoped to Cloud Firewalls"},
	} {
		if have(s.name, true) {
			continue
		}
		fmt.Printf("\n[optional] %s — %s\n", s.name, s.desc)
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
	fmt.Printf("\nWrote %d secret(s) + %d variable(s) to .llz/\n", len(secrets), len(vars))

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
		fmt.Println("\n(no --yes: nothing was created or pushed — re-run with --yes to execute)")
	}
	return nil
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
	fmt.Println("\n  Object Storage clusters:")
	for _, c := range clusters {
		id, _ := c["id"].(string)
		region, _ := c["region"].(string)
		status, _ := c["status"].(string)
		fmt.Printf("    %-14s region=%-12s %s\n", id, region, status)
	}
	fmt.Println("  (tip: pick the legacy \"-1\" cluster for your region — the Terraform provider rejects newer ones)")
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
	fmt.Printf("\nConfigure %s:\n", repo)
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
	for _, it := range items {
		if err := execArgv(it.argv, it.val); err != nil {
			return fmt.Errorf("%s: %w", it.argv[3], err)
		}
	}
	// Restrict infra-<env> secret injection to ref=main (the real boundary that
	// stops a feature-branch dispatch from exfiltrating the OpenBao unseal keys).
	return lockInfraEnvBranchPolicy(g, repo, env)
}

// configureTemplateHarness sets the template repo's e2e vars + E2E_DISPATCH_TOKEN
// (skipping anything already set).
func configureTemplateHarness(g globalOpts, in *bufio.Scanner, instanceRepo, clusterID string, st liveState) error {
	tr := templateRepo()
	fmt.Printf("\n[admin] e2e harness on %s\n", tr)
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
		fmt.Println("    • E2E_DISPATCH_TOKEN already set — skipping")
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
