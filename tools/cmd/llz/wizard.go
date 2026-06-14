package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
)

// secretSpec is one credential the token wizard requests. Dest records where the
// value belongs once pushed; URL is the page to mint it on (pre-filled where the
// provider supports query params); IsVar marks a non-secret GitHub repo variable.
type secretSpec struct {
	Name    string
	Purpose string
	Dest    string
	URL     string
	Note    string
	IsVar   bool
}

// Provider link builders. GitHub honors ?scopes=&description= on the new-token
// page; Linode does not, so those link to the right page with scopes printed.
const (
	linodeTokensURL  = "https://cloud.linode.com/profile/tokens"
	linodeObjKeysURL = "https://cloud.linode.com/object-storage/access-keys"
	linodeBucketsURL = "https://cloud.linode.com/object-storage/buckets"
)

func ghTokenURL(scopes, desc string) string {
	return "https://github.com/settings/tokens/new?scopes=" + scopes + "&description=" + desc
}

// ghFineGrainedTokenURL builds a template URL for the fine-grained PAT creation
// page (github.blog/changelog/2025-08-26). It pre-fills the token name,
// description, resource owner (target_name), a 90-day expiry (matches the
// ≤90-day rotation policy in runbooks/linode-credential-rotation.md), and the
// Contents: write repository permission apl-operator needs. GitHub does NOT
// support pre-selecting WHICH repository via URL params, so the caller's note
// must tell the operator to pick the specific repo under "Only select
// repositories". owner may be "" (the operator then picks the resource owner).
func ghFineGrainedTokenURL(name, owner, desc string) string {
	q := url.Values{}
	q.Set("name", name)
	if desc != "" {
		q.Set("description", desc)
	}
	if owner != "" {
		q.Set("target_name", owner)
	}
	q.Set("expires_in", "90")
	q.Set("contents", "write")
	return "https://github.com/settings/personal-access-tokens/new?" + q.Encode()
}

// ghFineGrainedPackagesURL builds a fine-grained PAT creation URL pre-filled for
// reading org packages (GHCR): name, resource owner, 90-day expiry. GitHub can't
// pre-select the Packages permission via query, so the caller tells the operator
// to set "Packages: Read-only" on the page.
func ghFineGrainedPackagesURL(name, owner string) string {
	q := url.Values{}
	q.Set("name", name)
	if owner != "" {
		q.Set("target_name", owner)
	}
	q.Set("expires_in", "90")
	return "https://github.com/settings/personal-access-tokens/new?" + q.Encode()
}

// ghFineGrainedDispatchURL builds a fine-grained PAT creation URL pre-filled for
// the e2e dispatch token: name, resource owner, 90-day expiry, and the three
// repository permissions the e2e run needs — Contents (force-push the
// instantiated tree), Actions (workflow_dispatch + watch the runs), and
// Workflows (the force-push rewrites .github/workflows/*). GitHub can't
// pre-select WHICH repository via query, so the caller tells the operator to
// pick it under "Only select repositories". Unknown perm keys are harmlessly
// ignored by GitHub, so the operator confirms the toggles on the page anyway.
func ghFineGrainedDispatchURL(name, owner string) string {
	q := url.Values{}
	q.Set("name", name)
	if owner != "" {
		q.Set("target_name", owner)
	}
	q.Set("expires_in", "90")
	q.Set("contents", "write")
	q.Set("actions", "write")
	q.Set("workflows", "write")
	return "https://github.com/settings/personal-access-tokens/new?" + q.Encode()
}

// catalog is the credential set the wizard walks. It mirrors docs/quickstart.md
// §2 and runbooks/bootstrap-openbao.md — and deliberately OMITS the secrets the
// build writes for you (OPENBAO_UNSEAL_KEY_*, LOKI_S3_*, HARBOR_*, AppRole IDs).
func catalog() []secretSpec {
	return []secretSpec{
		{
			Name:    "LINODE_API_TOKEN",
			Purpose: "Linode API token — Terraform provisioning + bootstrap",
			Dest:    "infra-<env> environment secret",
			URL:     linodeTokensURL,
			Note:    "Personal Access Token, Read/Write on Linodes, VPCs, Object Storage, Firewalls, Kubernetes. Expiry ≤ 90 days.",
		},
		{
			Name:    "OPENBAO_SECRETS_WRITE_TOKEN",
			Purpose: "GitHub PAT — CI stashes OBJ keys + persists OpenBao unseal keys / ESO AppRole",
			Dest:    "infra-<env> environment secret",
			URL:     ghTokenURL("repo,workflow", "llz-openbao-secrets-write"),
			Note:    "Classic PAT, scopes repo+workflow. You must ALSO be Environment admin on every infra-<env> environment, or --env writes 401.",
		},
		{
			Name:    "APL_VALUES_REPO_TOKEN",
			Purpose: "GitHub PAT — apl-core's external values store (otomi.git) + the argocd repo Secrets; apl-operator PUSHES its values tree here",
			Dest:    "infra-<env> environment secret",
			URL:     ghFineGrainedTokenURL("llz-apl-values-repo", "", "apl-core values repo (otomi.git) + argocd repo Secrets"),
			Note:    "Fine-grained PAT (Contents: write pre-filled) → set Resource owner to your org, then Only select repositories: your instance repo. The in-cluster Gitea is obsoleted; this is the only values-repo credential.",
		},
		{
			Name:    "TF_STATE_ACCESS_KEY",
			Purpose: "S3 access key for the Terraform-state bucket",
			Dest:    "infra-<env> environment secret",
			URL:     linodeObjKeysURL,
		},
		{
			Name:    "TF_STATE_SECRET_KEY",
			Purpose: "S3 secret key for the Terraform-state bucket",
			Dest:    "infra-<env> environment secret",
			URL:     linodeObjKeysURL,
		},
		{
			Name:    "TF_VAR_github_token",
			Purpose: "Read token for your ACL IP-inventory repo",
			Dest:    "CI secret (TF_VAR_github_token)",
			URL:     ghTokenURL("repo", "llz-acl-inventory-read"),
			Note:    "Read-only on the inventory repo (a fine-grained 'Contents: read' token also works).",
		},
		{
			Name:    "LINODE_DNS_TOKEN",
			Purpose: "Linode token scoped to DNS write (cert-manager DNS-01) — optional now",
			Dest:    "infra-<env> environment secret",
			URL:     linodeTokensURL,
			Note:    "Domains: Read/Write ONLY — narrower than LINODE_API_TOKEN. Cluster reaches a working state without it; ACME certs fail until provisioned.",
		},
		{
			Name:    "TF_STATE_BUCKET",
			Purpose: "Terraform-state bucket name",
			Dest:    "repository variable",
			URL:     linodeBucketsURL,
			IsVar:   true,
		},
		{
			Name:    "TF_STATE_ENDPOINT",
			Purpose: "S3-compatible endpoint URL for the state bucket",
			Dest:    "repository variable",
			URL:     linodeBucketsURL,
			IsVar:   true,
		},
		{
			Name:    "HARBOR_URL",
			Purpose: "Harbor registry base URL (e.g. harbor.<cluster_domain>)",
			Dest:    "repository variable",
			IsVar:   true,
		},
	}
}

// gather walks the catalog interactively, writing values into dir/.llz. In
// dry-run it prints the catalog + links and writes nothing.
func gather(g globalOpts, dir string) error {
	specs := catalog()

	fmt.Println("\nToken wizard — create each credential at the link shown, then paste it.")
	fmt.Println("Values are written to .llz/ (0600, gitignored) and never committed.")
	if g.open {
		fmt.Println("(--open: each link opens in your browser)")
	}

	if g.dryRun {
		for _, s := range specs {
			printSpec(s)
		}
		fmt.Println("\n(dry-run: no values requested, nothing written)")
		return nil
	}

	llzDir := filepath.Join(dir, ".llz")
	if err := os.MkdirAll(llzDir, 0o700); err != nil {
		return err
	}
	secretsPath := filepath.Join(llzDir, "secrets.env")
	varsPath := filepath.Join(llzDir, "vars.env")
	secrets := readEnvFile(secretsPath)
	vars := readEnvFile(varsPath)

	in := bufio.NewScanner(os.Stdin)
	for _, s := range specs {
		printSpec(s)
		if s.URL != "" {
			openURL(g, s.URL)
		}
		fmt.Print("  value (Enter to skip): ")
		if !in.Scan() {
			break
		}
		v := strings.TrimSpace(in.Text())
		if v == "" {
			continue
		}
		if s.IsVar {
			vars[s.Name] = v
		} else {
			secrets[s.Name] = v
		}
	}

	if err := writeEnvFile(secretsPath, secrets); err != nil {
		return err
	}
	if err := writeEnvFile(varsPath, vars); err != nil {
		return err
	}
	fmt.Printf("\nWrote %d secret(s) to %s and %d variable(s) to %s.\n",
		len(secrets), secretsPath, len(vars), varsPath)
	fmt.Println("Push them with:  llz secrets push <env> --yes")
	return nil
}

func printSpec(s secretSpec) {
	fmt.Printf("\n• %s\n    %s\n    → %s\n", s.Name, s.Purpose, s.Dest)
	if s.URL != "" {
		fmt.Printf("    create: %s\n", s.URL)
	}
	if s.Note != "" {
		fmt.Printf("    note:   %s\n", s.Note)
	}
}

// renderEnvFile serializes m as sorted KEY=value lines (pure; tested).
func renderEnvFile(m map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// writeEnvFile writes m to path at 0600.
func writeEnvFile(path string, m map[string]string) error {
	if err := os.WriteFile(path, []byte(renderEnvFile(m)), 0o600); err != nil {
		return err
	}
	// WriteFile only applies perms on create; enforce on an existing file too.
	return os.Chmod(path, 0o600)
}

// readEnvFile parses KEY=value lines, ignoring blanks and # comments. Missing
// file → empty map.
func readEnvFile(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = v
		}
	}
	return m
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func openURL(g globalOpts, url string) {
	if !g.open {
		return
	}
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	if _, err := execLookPath(bin); err != nil {
		return
	}
	_ = exec.Command(bin, url).Start()
}

// ── secrets / doctor commands ────────────────────────────────────────────────

// pushSecrets writes the gathered values into the infra-<env> GitHub
// Environment (secrets) and repo variables, then locks the env branch policy.
// Cloud-mutating: prints the plan and executes only with --yes. Secret VALUES
// are piped via stdin, never placed in argv, so even the printed plan is safe.
func pushSecrets(g globalOpts, env string) error {
	if err := validateEnvName(env); err != nil {
		return err
	}
	secrets := readEnvFile(filepath.Join(".llz", "secrets.env"))
	vars := readEnvFile(filepath.Join(".llz", "vars.env"))
	if len(secrets)+len(vars) == 0 {
		return fmt.Errorf("nothing to push — run `llz secrets gather` first")
	}

	f := forgeFn("")
	var writes []forgeWrite
	for _, k := range sortedKeys(secrets) {
		writes = append(writes, forgeWrite{name: k, value: secrets[k], secret: true, scope: scopeFor("infra-" + env)})
	}
	for _, k := range sortedKeys(vars) {
		writes = append(writes, forgeWrite{name: k, value: vars[k], scope: scopeFor("")})
	}
	echoWrites(writes)

	if g.dryRun {
		_ = lockInfraEnvBranchPolicy(g, "", env) // prints the plan, changes nothing
		return nil
	}
	if !g.yes {
		fmt.Fprintln(os.Stderr, "→ lock infra-"+env+" branch policy to main")
		fmt.Fprintln(os.Stderr, "  (re-run with --yes to execute)")
		return nil
	}
	if _, err := applyWrites(g, f, writes); err != nil {
		return err
	}
	// Lock infra-<env> to main-only — the actual secret-injection boundary.
	return lockInfraEnvBranchPolicy(g, "", env)
}

// runDoctor is the single "am I ready to build?" gate: it reports tooling + gh
// auth, then the file-level deployment readiness (the former `llz validate --env`)
// and the e2e/repo-config readiness, aggregating every failure so one run shows
// all the blockers. envExplicit distinguishes a user-supplied --env from the
// default, so a bare `llz doctor` doesn't error on a scaffold that was never added.
func runDoctor(repo, env string, admin, envExplicit bool, sshHost, knownHosts string) error {
	fmt.Println("Tooling:")
	// terraform OR tofu satisfies the Terraform requirement.
	reportEither("terraform", "tofu")
	for _, t := range []string{"copier", "gh", "kubectl", "helm", "bao", "jq", "linode-cli"} {
		report(t, lookable(t))
	}

	// Check the active forge's CLI (glab for GitLab, else gh). Both expose
	// `<cli> auth status`.
	cli := "gh"
	if forgeFn("").Flavor() == forge.GitLab {
		cli = "glab"
	}
	fmt.Printf("\nForge auth (%s):\n", cli)
	if _, err := execLookPath(cli); err != nil {
		report(cli+" auth status", false)
	} else {
		_, err := execOutput(cli, "auth", "status")
		report(cli+" auth status", err == nil)
	}

	var errs []error

	// Opt-in SSH host reachability + known_hosts freshness (an SSH-based GitOps
	// source path). Runs only when --ssh-host is given so it adds no noise.
	if sshHost != "" {
		if err := checkSSHHost(sshHost, "22", knownHosts); err != nil {
			errs = append(errs, err)
		}
	}

	// File-level deployment readiness (the former `llz validate --env`): scans the
	// tfvars + overlay for unfilled scaffold placeholders and renders the overlay.
	// Folding it in makes doctor the one readiness gate. Run it whenever the env was
	// asked for explicitly, or a scaffold for the default env already exists — so a
	// bare `llz doctor` stays quiet when no deployment has been scaffolded.
	if env != "" && (envExplicit || scaffoldExists(env)) {
		fmt.Println()
		if err := runEnvReadiness(env); err != nil {
			errs = append(errs, err)
		}
	}

	// e2e readiness — .llz/*.env merged with the live repo config. Needs a repo:
	// the flag, an instance's .copier-answers.yml, or --admin (the example repo).
	if repo == "" && !admin {
		if a, _ := readAnswers("."); a == nil || a.InstanceRepo == "" {
			fmt.Println("\ne2e readiness: pass --repo <owner>/<name>, run inside an instance, or use --admin.")
			return errors.Join(errs...)
		}
	}
	if err := cmdDoctorE2E(repo, env, admin); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func lookable(bin string) bool {
	_, err := execLookPath(bin)
	return err == nil
}

func reportEither(a, b string) {
	report(a+" or "+b, lookable(a) || lookable(b))
}

func report(name string, ok bool) {
	mark := "✗"
	if ok {
		mark = "✓"
	}
	fmt.Printf("  %s  %s\n", mark, name)
}
