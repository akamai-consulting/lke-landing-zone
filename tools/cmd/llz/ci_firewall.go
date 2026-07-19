package main

// ci_firewall.go implements `llz ci bootstrap-cloud-firewall` — the native port
// of bootstrap-cloud-firewall.sh, the canonical cloud-firewall bootstrap shared
// by .github/workflows/release.yml and .github/workflows/terraform.yml so the
// two pipelines cannot drift.
//
// Seeds the in-cluster state required by the custom firewall-controller
// (tools/firewall-controller) to reconcile the node-pool Linode Cloud Firewall
// in place:
//  1. Linode API Secret (kube-system/linode, key=token).
//  2. linode-internal-cidr-firewall-config ConfigMap, with LINODE_FIREWALL_ID
//     (the TF node_firewall_id) and optionally LKE_CLUSTER_ID.
//
// The controller does NOT create a CloudFirewall CR or use the upstream Linode
// cloud-firewall-controller — Linode caps Cloud Firewalls at one per linode,
// and Terraform's llz-node-firewall module already created and attached the
// firewall. The controller just edits that firewall's ruleset on every
// reconcile via PUT /v4/networking/firewalls/{id}/rules.
//
// Idempotent — safe to re-run. One deliberate improvement over the script: the
// script put the token on `kubectl create secret --from-literal` argv; here the
// manifests are rendered in Go and ride stdin to `kubectl apply -f -`, so the
// token never appears in a process listing.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

// firewallConfigMapName is the controller's config ConfigMap (kube-system). It
// MUST match the chart's fullname-derived name (release llz-linode-cidr-firewall
// → <fullname>-config), which is what the Deployment's env reads, so the dynamic
// LINODE_FIREWALL_ID / LKE_CLUSTER_ID we patch here land in the ConfigMap the
// controller actually consumes. The controller + chart live in the private
// lke-landing-zone-internal repo; these llz subcommands are the integration hook
// that bootstraps and health-checks it. The Application ignoreDifferences those
// two keys so selfHeal keeps our patch (the chart renders them empty placeholders).
const firewallConfigMapName = "llz-linode-cidr-firewall-config"

// firewallDeploymentName is the controller Deployment (chart fullname). After
// patching the ConfigMap we roll it: env injected via configMapKeyRef is read
// once at pod creation, so a Deployment ArgoCD already created from the empty
// placeholders would crashloop on the stale values until restarted.
const firewallDeploymentName = "llz-linode-cidr-firewall"

// firewallKubectlFn runs kubectl with args, piping stdin (a rendered manifest /
// patchless empty string) to it and streaming output. KUBECONFIG reaches
// kubectl through the inherited environment, exactly as the script's
// `export KUBECONFIG` did. Seamed for tests.
var firewallKubectlFn = func(stdin string, args ...string) error {
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func ciBootstrapCloudFirewallCmd() *cobra.Command {
	var region string
	cmd := &cobra.Command{
		Use:   "bootstrap-cloud-firewall",
		Short: "seed the firewall-controller's token Secret + config ConfigMap on the cluster",
		Long: "Native port of bootstrap-cloud-firewall.sh. Seeds the in-cluster state the\n" +
			"custom firewall-controller (tools/firewall-controller) needs to reconcile the\n" +
			"node-pool Linode Cloud Firewall in place: the kube-system/linode API-token\n" +
			"Secret (key=token) and the linode-internal-cidr-firewall-config ConfigMap\n" +
			"with LINODE_FIREWALL_ID (plus LKE_CLUSTER_ID when CLUSTER_ID is set, enabling\n" +
			"control-plane ACL reconciliation). Idempotent — safe to re-run.\n\n" +
			"--region <prefix> resolves LINODE_FIREWALL_ID, CLUSTER_ID and VPC_CIDR from\n" +
			"<prefix>.tfvars + the Linode API (firewall + cluster by label, the subnet\n" +
			"CIDR from tfvars), replacing the former `terraform init`+`terraform output`\n" +
			"round-trip against the cluster workspace. Explicit env values still win.\n\n" +
			"Env: KUBECONFIG (an existing, non-empty kubeconfig) is always required;\n" +
			"LINODE_FIREWALL_ID is required unless --region resolves it. The Secret token\n" +
			"comes from CLOUD_FIREWALL_TOKEN (preferred least-privilege scope:\n" +
			"firewall:read_write on the node firewall) with LINODE_TOKEN as the\n" +
			"warned-about fallback; CLUSTER_ID is optional. --region label resolution uses\n" +
			"LINODE_TOKEN (or LINODE_API_TOKEN), which needs account read scope.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if region != "" {
				if err := resolveFirewallInputsIntoEnv(region); err != nil {
					return err
				}
			}
			return runCIBootstrapCloudFirewall()
		},
	}
	cmd.Flags().StringVar(&region, "region", "", "tfvars prefix (e.g. primary); resolve LINODE_FIREWALL_ID / CLUSTER_ID / VPC_CIDR from <region>.tfvars + the Linode API instead of the environment")
	return cmd
}

// firewallResolveFn resolves the node firewall ID and LKE cluster ID by label via
// the Linode API. Seamed so tests exercise resolveFirewallInputsIntoEnv without a
// live account.
var firewallResolveFn = func(token string, labels tf.Labels) (firewallID, clusterID string, err error) {
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()

	fws, err := client.ListFirewalls(ctx)
	if err != nil {
		return "", "", fmt.Errorf("list firewalls: %w", err)
	}
	fid, ok := linode.FindIDByLabel(fws, labels.Firewall)
	if !ok {
		return "", "", fmt.Errorf("no Linode Cloud Firewall labelled %q found — was the cluster workspace applied?", labels.Firewall)
	}

	ids, err := client.ClustersWithLabel(ctx, labels.Cluster)
	if err != nil {
		return "", "", fmt.Errorf("list LKE clusters labelled %q: %w", labels.Cluster, err)
	}
	if len(ids) == 0 {
		return "", "", fmt.Errorf("no LKE cluster labelled %q found — was the cluster workspace applied?", labels.Cluster)
	}
	return strconv.FormatUint(fid, 10), strconv.FormatUint(ids[0], 10), nil
}

// resolveFirewallInputsIntoEnv derives the firewall-controller inputs from
// <region>.tfvars + the Linode API and writes them into the environment that
// runCIBootstrapCloudFirewall reads. Replaces the workflow's `terraform init`
// (cluster module) + three `terraform output` reads of remote state — the
// firewall/cluster IDs come from the API by their account-unique labels and the
// VPC subnet CIDR straight from tfvars. Any value already set in the environment
// is left untouched, so an explicit override still wins.
func resolveFirewallInputsIntoEnv(region string) error {
	token, err := ciToken()
	if err != nil {
		return fmt.Errorf("%w — needed so --region can resolve the firewall + cluster IDs by label", err)
	}

	// tfvars file: prefer <region>.tfvars, fall back to the .example — mirrors
	// runCITFImport so resolution works in the same working dirs.
	varFile := region + ".tfvars"
	if _, err := os.Stat(varFile); err != nil {
		varFile = region + ".tfvars.example"
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", varFile, err)
	}
	vars := tf.ParseTFVars(string(content))
	if vars.ClusterLabel == "" {
		return fmt.Errorf("%s has no cluster_label", varFile)
	}

	fid, cid, err := firewallResolveFn(token, tf.DeriveLabels(vars))
	if err != nil {
		return err
	}
	setenvIfEmpty("LINODE_FIREWALL_ID", fid)
	setenvIfEmpty("CLUSTER_ID", cid)
	setenvIfEmpty("VPC_CIDR", vars.VPCSubnetCIDR)
	return nil
}

// setenvIfEmpty sets an environment variable only when it is currently unset or
// empty, so an explicit value passed by the caller is never clobbered by the
// --region resolution.
func setenvIfEmpty(key, value string) {
	if os.Getenv(key) == "" {
		_ = os.Setenv(key, value)
	}
}

func runCIBootstrapCloudFirewall() error {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		return fmt.Errorf("KUBECONFIG must be set")
	}
	firewallID := os.Getenv("LINODE_FIREWALL_ID")
	if firewallID == "" {
		return fmt.Errorf("LINODE_FIREWALL_ID must be set (numeric Linode Cloud Firewall ID; pass the cluster workspace node_firewall_id output)")
	}
	if fi, err := os.Stat(kubeconfig); err != nil || fi.Size() == 0 {
		fmt.Fprintf(os.Stderr, "::error::KUBECONFIG %s is missing or empty. The fetch-kubeconfig step did not produce a kubeconfig — the cluster Terraform state likely has no populated kubeconfig_raw (cluster not provisioned, or imported without a refresh).\n", kubeconfig)
		return fmt.Errorf("kubeconfig %s is missing or empty", kubeconfig)
	}

	// ── Resolve the API token (CLOUD_FIREWALL_TOKEN, else LINODE_TOKEN) ─────────
	token := os.Getenv("CLOUD_FIREWALL_TOKEN")
	if token == "" {
		token = os.Getenv("LINODE_TOKEN")
		if token == "" {
			fmt.Fprintln(os.Stderr, "::error::Neither CLOUD_FIREWALL_TOKEN nor LINODE_TOKEN is set — the firewall-controller cannot authenticate to the Linode API.")
			return fmt.Errorf("no Linode API token in CLOUD_FIREWALL_TOKEN or LINODE_TOKEN")
		}
		fmt.Fprintln(os.Stderr, "::warning::CLOUD_FIREWALL_TOKEN not set — falling back to LINODE_TOKEN. Set a least-privilege CLOUD_FIREWALL_TOKEN with firewall:read_write on the node firewall when possible.")
	}

	// ── Seed the controller's token Secret ────────────────────────────────────
	// Mounted as env LINODE_TOKEN by the linode-internal-cidr-firewall
	// Deployment. The manifest rides stdin so the token stays off argv.
	if err := firewallKubectlFn(firewallSecretManifest(token), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply kube-system/linode Secret: %w", err)
	}

	// ── Seed the controller's ConfigMap ───────────────────────────────────────
	// The linode-internal-cidr-firewall Helm chart renders this ConfigMap with
	// LINODE_FIREWALL_ID="" and LKE_CLUSTER_ID="" as placeholders; this command
	// patches the real values in. ArgoCD treats these fields as bootstrap-owned:
	// the Argo Application syncs with ServerSideApply, so the values written
	// here under a different SSA field manager survive selfHeal (Argo does not
	// own them).
	if err := firewallKubectlFn(firewallConfigMapManifest(), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply kube-system/%s ConfigMap: %w", firewallConfigMapName, err)
	}

	if err := patchFirewallConfig("LINODE_FIREWALL_ID", firewallID); err != nil {
		return err
	}

	// VPC_CIDR is the cluster's actual VPC subnet CIDR (cluster workspace
	// vpc_subnet_cidr output) — the SAME range the TF node-firewall's intra-VPC
	// allow rules use. Patching it here keeps the runtime controller's VPC_CIDR
	// in lock-step with the VPC it manages, instead of the chart's stale default
	// (a /16 that excludes the LKE pod/service CIDRs and black-holes cross-node
	// traffic). Optional: an empty value leaves the chart default in place.
	if vpcCIDR := os.Getenv("VPC_CIDR"); vpcCIDR != "" {
		if err := patchFirewallConfig("VPC_CIDR", vpcCIDR); err != nil {
			return err
		}
	}

	if clusterID := os.Getenv("CLUSTER_ID"); clusterID != "" {
		if err := patchFirewallConfig("LKE_CLUSTER_ID", clusterID); err != nil {
			return err
		}
	} else {
		fmt.Println("CLUSTER_ID not provided — LKE control-plane ACL reconciliation will be disabled")
	}

	// Roll the Deployment so a pod ArgoCD created from the chart's empty
	// placeholders picks up the values just patched in (configMapKeyRef env is
	// read once at pod creation). A "not found" is benign — ArgoCD has not synced
	// the Deployment yet, so it will start fresh from the already-patched ConfigMap.
	if err := firewallKubectlFn("", "rollout", "restart", "deployment", firewallDeploymentName, "-n", "kube-system"); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::could not roll %s after patching its ConfigMap (likely not created by ArgoCD yet; it will start from the patched values): %v\n", firewallDeploymentName, err)
	}
	return nil
}

// firewallSecretManifest renders the kube-system/linode Secret carrying the
// Linode API token (key=token) as a JSON manifest — `kubectl apply` accepts
// JSON, and encoding/json keeps the token correctly escaped. stringData (not
// data) so the API server does the base64 encoding, matching what the script's
// `kubectl create secret generic --from-literal` produced.
func firewallSecretManifest(token string) string {
	// json.Marshal of string-keyed maps of strings cannot fail.
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]string{"name": "linode", "namespace": "kube-system"},
		"type":       "Opaque",
		"stringData": map[string]string{"token": token},
	})
	return string(b)
}

// firewallConfigMapManifest renders the empty controller ConfigMap; the real
// values are merge-patched in afterwards (see the SSA comment above).
func firewallConfigMapManifest() string {
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]string{"name": firewallConfigMapName, "namespace": "kube-system"},
	})
	return string(b)
}

// firewallConfigPatch builds the merge-patch body {"data":{key:value}}.
func firewallConfigPatch(key, value string) string {
	b, _ := json.Marshal(map[string]map[string]string{"data": {key: value}})
	return string(b)
}

// patchFirewallConfig merge-patches one data key into the controller ConfigMap
// and logs it, mirroring the script's per-key `kubectl patch` + echo.
func patchFirewallConfig(key, value string) error {
	if err := firewallKubectlFn("", "patch", "configmap", firewallConfigMapName,
		"-n", "kube-system", "--type", "merge", "--patch", firewallConfigPatch(key, value)); err != nil {
		return fmt.Errorf("patch %s into %s: %w", key, firewallConfigMapName, err)
	}
	fmt.Printf("Set %s=%s in %s\n", key, value, firewallConfigMapName)
	return nil
}
