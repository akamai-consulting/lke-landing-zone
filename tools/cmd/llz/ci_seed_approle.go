package main

// ci_seed_approle.go implements `llz ci seed-approle` — the native port of the
// "Seed ESO AppRole secret and GitHub CI secrets" and "Seed secret-propagator
// AppRole credentials" steps of llz-bootstrap-openbao.yml. Both steps mint a
// fresh secret_id for an AppRole created by `llz ci bao-configure`, mask it,
// and fan it out: the ESO variant also applies a K8s Secret (ESO reads
// secretId to authenticate with OpenBao) and writes repo-level GH secrets with
// the HA-aware _STANDBY suffix; the propagator variant writes env-scoped GH
// secrets on infra-<region>. The secret_id rides `gh secret set` stdin and a
// K8s Secret manifest piped to `kubectl apply -f -` — never a local argv.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// kubectlApplyFn pipes a rendered manifest to `kubectl apply -f -` — the
// native form of the scripts' `kubectl create … --dry-run=client -o yaml |
// kubectl apply -f -` idempotent-apply idiom. Seamed for tests.
var kubectlApplyFn = func(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// parseApproleSecretID extracts .data.secret_id from
// `bao write -f auth/approle/role/<role>/secret-id -format=json`.
func parseApproleSecretID(out string) string {
	var r struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return ""
	}
	return r.Data.SecretID
}

// chooseApproleGHSecret picks the GH secret name for the minted secret_id:
// the standby peer writes the _STANDBY-suffixed name (same repo-level naming
// convention as the approle-rotation CronWorkflow, so secrets stay consistent
// after the first rotation).
func chooseApproleGHSecret(base, standby, haRole string) string {
	if haRole == "standby" && standby != "" {
		return standby
	}
	return base
}

// parseK8sSecretRef parses a --k8s-secret NS/NAME:KEY spec.
func parseK8sSecretRef(spec string) (ns, name, key string, err error) {
	loc, key, ok := strings.Cut(spec, ":")
	ns, name, ok2 := strings.Cut(loc, "/")
	if !ok || !ok2 || ns == "" || name == "" || key == "" {
		return "", "", "", fmt.Errorf("--k8s-secret must be NAMESPACE/NAME:KEY, got %q", spec)
	}
	return ns, name, key, nil
}

// genericSecretManifest renders an Opaque Secret with one key. The value is
// base64-encoded into `data:` so no YAML escaping of the secret is needed.
func genericSecretManifest(ns, name, key, value string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  %s: %s
`, name, ns, key, base64.StdEncoding.EncodeToString([]byte(value)))
}

// mintApproleSecretID mints a fresh secret_id for an AppRole through the
// in-pod bao CLI (root token, like the `llz openbao exec` the bash shelled).
func mintApproleSecretID(role string) (string, error) {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return "", fmt.Errorf("OPENBAO_ROOT_TOKEN must be set (minting an AppRole secret-id runs through the in-pod bao CLI)")
	}
	out, errOut, err := baoExecFn(rootOpenbaoPod, token, "",
		"write", "-f", "auth/approle/role/"+role+"/secret-id", "-format=json")
	if err != nil {
		return "", fmt.Errorf("mint secret-id for AppRole %s: %s", role, strings.TrimSpace(firstNonEmpty(errOut, out)))
	}
	id := parseApproleSecretID(out)
	if id == "" {
		return "", fmt.Errorf("AppRole %s secret-id mint returned no .data.secret_id", role)
	}
	return id, nil
}

type seedApproleOpts struct {
	role            string
	k8sSecret       string
	ghRoleSecret    string
	ghSecret        string
	ghSecretStandby string
	ghEnv           string
	summary         []string
	doneMessage     string
}

func ciSeedApproleCmd() *cobra.Command {
	var o seedApproleOpts
	c := &cobra.Command{
		Use:   "seed-approle",
		Short: "mint an AppRole secret-id and seed it to a K8s Secret and/or GitHub secrets",
		Long: "Native port of the AppRole seed steps of llz-bootstrap-openbao.yml. Mints a\n" +
			"fresh secret_id for --role via `bao write -f auth/approle/role/<role>/\n" +
			"secret-id`, masks it, and fans it out: --k8s-secret NS/NAME:KEY applies an\n" +
			"Opaque Secret holding it (the eso-approle-secret ESO authenticates with);\n" +
			"--gh-role-secret stores the role name and --gh-secret the secret_id as\n" +
			"GitHub secrets (--gh-secret-standby is used instead when HA_ROLE=standby —\n" +
			"the rotation CronWorkflow's _STANDBY naming); --gh-env scopes the gh writes\n" +
			"to an Environment (the secret-propagator variant). Values ride stdin /\n" +
			"manifest data, never a local argv. Reads OPENBAO_ROOT_TOKEN, HA_ROLE,\n" +
			"GH_TOKEN/GH_REPO.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCISeedApprole(o) },
	}
	f := c.Flags()
	f.StringVar(&o.role, "role", "", "AppRole name to mint a secret-id for, e.g. platform-ci (required)")
	f.StringVar(&o.k8sSecret, "k8s-secret", "", "apply the secret-id as NAMESPACE/NAME:KEY (e.g. llz-external-secrets/eso-approle-secret:secretId)")
	f.StringVar(&o.ghRoleSecret, "gh-role-secret", "", "GitHub secret name receiving the role name")
	f.StringVar(&o.ghSecret, "gh-secret", "", "GitHub secret name receiving the secret-id")
	f.StringVar(&o.ghSecretStandby, "gh-secret-standby", "", "GitHub secret name used instead of --gh-secret when HA_ROLE=standby")
	f.StringVar(&o.ghEnv, "gh-env", "", "scope the gh secret writes to this Environment (default: repo-level)")
	f.StringArrayVar(&o.summary, "summary", nil, "$GITHUB_STEP_SUMMARY line appended after seeding (repeatable)")
	f.StringVar(&o.doneMessage, "done-message", "", "stdout line after a successful seed")
	return c
}

// ghSeedSecret writes a GH secret repo-level or env-scoped depending on ghEnv,
// reusing the stdin-piping seams.
func ghSeedSecret(name, ghEnv, value string) error {
	if ghEnv != "" {
		return ghSetSecretFn(name, ghEnv, value)
	}
	return ghSetRepoSecretFn(name, value)
}

func runCISeedApprole(o seedApproleOpts) error {
	if o.role == "" {
		return fmt.Errorf("--role is required")
	}
	// Validate the K8s ref before minting, so a flag typo doesn't burn a
	// secret-id accumulation against the role's secret_id_num_uses budget.
	var ns, name, key string
	if o.k8sSecret != "" {
		var err error
		if ns, name, key, err = parseK8sSecretRef(o.k8sSecret); err != nil {
			return err
		}
	}

	secretID, err := mintApproleSecretID(o.role)
	if err != nil {
		return err
	}
	maskGHA(secretID)

	if o.k8sSecret != "" {
		if err := kubectlApplyFn(genericSecretManifest(ns, name, key, secretID)); err != nil {
			return fmt.Errorf("apply Secret %s/%s: %w", ns, name, err)
		}
	}
	if o.ghRoleSecret != "" {
		if err := ghSeedSecret(o.ghRoleSecret, o.ghEnv, o.role); err != nil {
			return err
		}
	}
	if ghName := chooseApproleGHSecret(o.ghSecret, o.ghSecretStandby, os.Getenv("HA_ROLE")); ghName != "" {
		if err := ghSeedSecret(ghName, o.ghEnv, secretID); err != nil {
			return err
		}
	}

	if o.doneMessage != "" {
		fmt.Println(o.doneMessage)
	}
	return appendGHAFile("GITHUB_STEP_SUMMARY", o.summary...)
}
