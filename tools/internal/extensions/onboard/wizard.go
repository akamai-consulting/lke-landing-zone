package onboard

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/branchpolicy"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/doctor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghcli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
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

// ghFineGrainedSecretsWriteURL builds a fine-grained PAT creation URL pre-filled
// for OPENBAO_SECRETS_WRITE_TOKEN: name, resource owner, 90-day expiry, and the
// repository permissions CI's `gh secret set --env` needs. GitHub can't
// pre-select WHICH repository via query, so the caller tells the operator to
// pick it under "Only select repositories". owner may be "" (the operator then
// picks the resource owner). A classic repo+workflow PAT also works; this is the
// least-privilege option.
//
// ENVIRONMENTS, not Secrets, is the permission that governs this token's actual
// job. This pre-fill used to request Actions + Secrets: write, which reads right
// and is WRONG: the fine-grained "Secrets" permission covers only repo-level
// Actions secrets (/repos/{o}/{r}/actions/secrets/...), while every secret in
// catalog() is destined for an infra-<env> ENVIRONMENT secret, and those
// endpoints — including the public-key GET that `gh secret set` must make before
// it can encrypt anything — sit under "Environments". So a PAT minted from the
// old link authenticated fine, passed every preflight, and then 403'd on the
// first environment-secret write of a first-ever bootstrap. Actions: write stays
// (the token is also used for workflow dispatch).
func ghFineGrainedSecretsWriteURL(name, owner string) string {
	q := url.Values{}
	q.Set("name", name)
	if owner != "" {
		q.Set("target_name", owner)
	}
	q.Set("expires_in", "90")
	q.Set("actions", "write")
	q.Set("environments", "write")
	return "https://github.com/settings/personal-access-tokens/new?" + q.Encode()
}

// catalog is the credential set the wizard walks. It mirrors docs/quickstart.md
// §2 and runbooks/bootstrap-openbao.md — and deliberately OMITS the secrets the
// build writes for you (OPENBAO_RECOVERY_KEY_*, LOKI_S3_*, HARBOR_*).
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
			Purpose: "GitHub PAT — CI stashes OBJ keys + persists OpenBao unseal keys",
			Dest:    "infra-<env> environment secret",
			URL:     ghFineGrainedSecretsWriteURL("llz-openbao-secrets-write", ""),
			Note:    "Fine-grained PAT, Actions: write + ENVIRONMENTS: write (set Resource owner to your org, then Only select repositories: your instance repo) — or a classic repo+workflow PAT. Environments is the permission that governs infra-<env> environment secrets; the \"Secrets\" permission covers only repo-level secrets and is NOT enough. You must ALSO be Environment admin on every infra-<env> environment.",
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
		{
			// The quickstart offers `llz secrets Gather` + `llz secrets push` as the
			// manual alternative for operators who would rather llz did not create
			// Linode resources for them. That path omitted this, so it produced an
			// instance whose every Terraform root exits 1 at init — `llz tokens`
			// generates it, but Gather does not run that code. Prompted rather than
			// generated here, because this catalog's whole contract is "you paste
			// every value yourself".
			Name:    statepassphrase.SecretName,
			Purpose: "OpenTofu state+plan encryption passphrase — REPO-LEVEL, one per instance. LEAVE BLANK if this instance already has one (`llz tokens` generates it); a NEW value here would make every existing state file unreadable. First time only: `openssl rand -base64 32`, and ESCROW IT",
			Dest:    "repository secret",
		},
	}
}

// Gather walks the catalog interactively, writing values into dir/.llz. In
// dry-run it prints the catalog + links and writes nothing.
func Gather(o Opts, dir string) error {
	specs := catalog()

	fmt.Println("\nToken wizard — create each credential at the link shown, then paste it.")
	fmt.Println("Values are written to .llz/ (0600, gitignored) and never committed.")
	if o.Open {
		fmt.Println("(--open: each link opens in your browser)")
	}

	if o.DryRun {
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
	secrets := ReadEnvFile(secretsPath)
	vars := ReadEnvFile(varsPath)

	in := bufio.NewScanner(os.Stdin)
	for _, s := range specs {
		printSpec(s)
		if s.URL != "" {
			openURL(o, s.URL)
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

	if err := WriteEnvFile(secretsPath, secrets); err != nil {
		return err
	}
	if err := WriteEnvFile(varsPath, vars); err != nil {
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
	for _, k := range SortedKeys(m) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// WriteEnvFile writes m to path at 0600.
func WriteEnvFile(path string, m map[string]string) error {
	if err := os.WriteFile(path, []byte(renderEnvFile(m)), 0o600); err != nil {
		return err
	}
	// WriteFile only applies perms on create; enforce on an existing file too.
	return os.Chmod(path, 0o600)
}

// ReadEnvFile parses KEY=value lines, ignoring blanks and # comments. Missing
// file → empty map.
func ReadEnvFile(path string) map[string]string {
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

func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func openURL(o Opts, url string) {
	if !o.Open {
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

// PushSecrets writes the gathered values into the infra-<env> GitHub
// Environment (secrets) and repo variables, then locks the env branch policy.
// Cloud-mutating: prints the plan and executes only with --yes. Secret VALUES
// are piped via stdin, never placed in argv, so even the printed plan is safe.
func PushSecrets(o Opts, env string) error {
	if err := validate.EnvName(env); err != nil {
		return err
	}
	secrets := ReadEnvFile(filepath.Join(".llz", "secrets.env"))
	vars := ReadEnvFile(filepath.Join(".llz", "vars.env"))
	if len(secrets)+len(vars) == 0 {
		return fmt.Errorf("nothing to push — run `llz secrets Gather` first")
	}

	// The manual route has the same clobber hazard as the wizard and none of its
	// guards: `Gather` re-prompts every catalog entry on every run, and the
	// passphrase's own Prompt text says `openssl rand -base64 32` — so an operator
	// re-running Gather to add one missing token can paste a NEW passphrase over
	// the live one and make every state file unreadable. Ask before pushing.
	if repo, rerr := answers.ResolveInstanceRepo("", false); rerr == nil {
		if err := statepassphrase.DropStatePassphraseIfLive(repo, env, secrets, false); err != nil {
			return err
		}
	}

	type item struct {
		argv []string
		val  string
	}
	var items []item
	for _, k := range SortedKeys(secrets) {
		items = append(items, item{ghcli.SecretSetArgv(env, k, envreq.SecretIsEnvScoped(k)), secrets[k]})
	}
	for _, k := range SortedKeys(vars) {
		items = append(items, item{ghcli.VariableSetArgv(k), vars[k]})
	}

	for _, it := range items {
		fmt.Fprintln(os.Stderr, "→ "+ghcli.Quote(it.argv))
	}

	if o.DryRun {
		_ = branchpolicy.Lock(o.DryRun, "", env) // prints the plan, changes nothing
		return nil
	}
	if !o.Yes {
		fmt.Fprintln(os.Stderr, "→ lock infra-"+env+" branch policy to main")
		fmt.Fprintln(os.Stderr, "  (re-run with --yes to execute)")
		return nil
	}
	// Create + lock infra-<env> BEFORE pushing — `gh secret set --env` 404s if the
	// environment doesn't exist yet, and branchpolicy.Lock is what creates
	// it. The lock is also the secret-injection boundary (main-only).
	protErr := branchpolicy.Lock(o.DryRun, "", env)
	if protErr != nil && !errors.Is(protErr, branchpolicy.ErrUnsupported) {
		return protErr
	}
	for _, it := range items {
		if err := proc.Run(it.argv, it.val); err != nil {
			return fmt.Errorf("%s: %w", it.argv[2], err) // argv[2] = the name
		}
	}
	if errors.Is(protErr, branchpolicy.ErrUnsupported) {
		branchpolicy.WarnUnsupported("", env)
	}
	return nil
}

// RunDoctor is the single "am I ready to build?" gate: it reports tooling + gh
// auth, then the file-level deployment readiness (the former `llz validate --env`)
// and the e2e/repo-config readiness, aggregating every failure so one run shows
// all the blockers. envExplicit distinguishes a user-supplied --env from the
// default, so a bare `llz doctor` doesn't error on a scaffold that was never added.
func RunDoctor(repo, env string, admin, envExplicit bool, sshHost, knownHosts string) error {
	fmt.Println(color.Bold("Tooling:"))
	// terraform OR tofu satisfies the Terraform requirement.
	reportEither("terraform", "tofu")
	// git is first because it is the one every other step assumes: copier clones
	// the template with it, `llz env add` commits with it, and the build reads what
	// `git push` published. It was the only tool in this flow that nothing checked
	// — the list carries `jq` and `linode-cli`, which llz never invokes at all.
	for _, t := range []string{"git", "copier", "gh", "kubectl", "helm", "bao", "jq", "linode-cli"} {
		report(t, kubectlprobe.Lookable(t))
	}

	fmt.Println("\n" + color.Bold("GitHub auth:"))
	if _, err := execLookPath("gh"); err != nil {
		report("gh auth status", false)
	} else {
		// Scope the check to the host llz actually uses. Bare `gh auth status`
		// exits non-zero if ANY configured host is broken (e.g. an expired GHE
		// token from an unrelated account), which would wrongly fail this gate
		// for a user who is properly logged in to github.com.
		host := ghcli.Host()
		_, err := execOutput("gh", "auth", "status", "--hostname", host)
		report("gh auth status ("+host+")", err == nil)
	}

	var errs []error

	// Cross-org reuse guardrail (#200): a thin-caller workflow whose reusable
	// `uses:` org differs from this instance's org while passing `secrets: inherit`
	// runs with EMPTY secrets — a silent setup-time trap. Fail loudly here.
	fmt.Println("\n" + color.Bold("Workflow reuse:"))
	if err := doctor.CheckCrossOrgReuse(); err != nil {
		errs = append(errs, err)
	}

	// Escape-hatch layout. `llz render` gates on this too, but doctor is the readiness
	// gate an operator runs FIRST — surfacing it here means they meet a reserved-name
	// mistake before a terraform op trips over it. See custom_layout.go.
	fmt.Println("\n" + color.Bold("Custom resources:"))
	tfDir, _, _ := instancelayout.Detect()
	customDir := filepath.Join(filepath.Dir(tfDir), clusterspec.CustomRoot)
	if err := instanceresolve.CheckCustomLayout(customDir); err != nil {
		report(clusterspec.CustomRoot+" layout", false)
		errs = append(errs, err)
	} else if _, statErr := os.Stat(customDir); statErr == nil {
		report(clusterspec.CustomRoot+" layout", true)
	} else {
		fmt.Printf("  (no %s/ tree in this repo — nothing to check)\n", clusterspec.CustomRoot)
	}

	// HA pairing. peerOf refuses to guess for the deployment it is ASKED about,
	// but nothing else checks the tree as a whole — and a group with two actives
	// still yields exactly one peer, so it slips past peerOf while making the
	// active/standby roles meaningless. doctor is the right place for the
	// whole-set rule: it reports, so a half-added pair (the expected state between
	// the two `llz env add` calls) is surfaced without failing anything.
	fmt.Println("\n" + color.Bold("HA topology:"))
	if deps, terr := envtopology.ReadTopology(tfDir); terr != nil {
		report("read cluster topology", false)
		errs = append(errs, terr)
	} else if len(envtopology.HAMembers(deps)) == 0 {
		fmt.Println("  (no HA pair declared — nothing to check)")
	} else if verr := envtopology.ValidateTopology(deps); verr != nil {
		report("active/standby pairing", false)
		fmt.Printf("     %s\n", verr)
	} else {
		report("active/standby pairing", true)
	}

	// Ask the ACCOUNT whether it can build what the spec pins. Everything above
	// this point is local or GitHub, so an unentitled Linode account or a retired
	// `+lke` pin used to reach terraform apply unchallenged. Advisory only — it
	// never touches errs. See doctor_linode.go.
	doctor.ReportLinodeAccount(doctor.SpecK8sVersions(env))

	// Opt-in SSH host reachability + known_hosts freshness (an SSH-based GitOps
	// source path). Runs only when --ssh-host is given so it adds no noise.
	if sshHost != "" {
		if err := reachability.CheckSSHHost(sshHost, "22", knownHosts); err != nil {
			errs = append(errs, err)
		}
	}

	// File-level deployment readiness (the former `llz validate --env`): scans the
	// tfvars + overlay for unfilled scaffold placeholders and renders the overlay.
	// Folding it in makes doctor the one readiness gate. Run it whenever the env was
	// asked for explicitly, or a scaffold for the default env already exists — so a
	// bare `llz doctor` stays quiet when no deployment has been scaffolded.
	if env != "" && (envExplicit || configreadiness.ScaffoldExists(env)) {
		fmt.Println()
		if err := configreadiness.RunEnvReadiness(env); err != nil {
			errs = append(errs, err)
		}
	}

	// The spec-answerable half of the CI preflights doctor used to leave to the
	// build — see doctor_build_preflights.go. Only inside an instance: outside one
	// there is no spec to check, and a bare `llz doctor` is a tooling check. The
	// image-pin half needs the REPO's variables, so it runs in cmdDoctorE2E below.
	if clusterspec.InstancePresent(filepath.Dir(tfDir)) {
		errs = append(errs, checkSpecPreflights(env)...)
	}

	// e2e readiness — .llz/*.env merged with the live repo config. Needs a repo:
	// the flag, an instance's .copier-answers.yml, or --admin (the example repo).
	if repo == "" && !admin {
		if a, _ := answers.Read("."); a == nil || a.InstanceRepo == "" {
			fmt.Println("\ne2e readiness: pass --repo <owner>/<name>, run inside an instance, or use --admin.")
			return errors.Join(errs...)
		}
	}
	if err := DoctorE2E(repo, env, admin); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func reportEither(a, b string) {
	report(a+" or "+b, kubectlprobe.Lookable(a) || kubectlprobe.Lookable(b))
}

func report(name string, ok bool) {
	mark := color.Red("✗")
	if ok {
		mark = color.Green("✓")
	}
	fmt.Printf("  %s  %s\n", mark, name)
}
