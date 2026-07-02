package main

// ci_seed_special.go implements the one-off seed/verify steps of
// llz-bootstrap-openbao.yml that don't fit the generic `llz ci bao-seed`
// shape (they derive their material instead of just relaying it):
//
//   seed-harbor-registry-s3  resolve obj_cluster from the object-storage
//                            tfvars and seed the registry's S3 backend creds
//   resolve-harbor-url       default HARBOR_URL to harbor.<domainSuffix> from
//                            the LandingZone spec
//   audit-pvc-storageclass   report PVCs that escaped the Kyverno encrypted-
//                            StorageClass mutation
//
// (seed-harbor-dockerconfig was retired: the harbor docker config.json is now
// derived in-cluster by the llz-cert-automation chart's harborDockerConfig
// ExternalSecret, which renders the dockerconfigjson from the robot creds in
// secret/harbor/robot via an ESO template — no separate seed/path.)

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// tfvarsValue returns the first `key = "value"` assignment in tfvars content
// (quotes stripped, comments ignored) — the same first-wins grep/sed
// semantics as internal/terraform.ParseTFVars, for keys outside its fixed
// struct (obj_cluster).
func tfvarsValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		i := strings.IndexByte(line, '=')
		if i < 0 || strings.TrimSpace(line[:i]) != key {
			continue
		}
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 && val[0] == '"' {
			if j := strings.IndexByte(val[1:], '"'); j >= 0 {
				return val[1 : 1+j]
			}
		}
		return val
	}
	return ""
}

// ── seed-harbor-registry-s3 ───────────────────────────────────────────────────

// harborRegistryS3Fields derives the five secret/harbor/registry-s3 fields.
// The bucket name encodes the deployment region (matches the TF resource
// label); endpoint/region come from the obj_cluster the object-storage tfvars
// actually provisioned into — NOT guessed from the env name.
func harborRegistryS3Fields(region, objCluster, accessKey, secretKey string) map[string]string {
	return map[string]string{
		"access_key_id":     accessKey,
		"secret_access_key": secretKey,
		"bucket_name":       "platform-harbor-registry-" + region,
		"endpoint":          "https://" + objCluster + ".linodeobjects.com",
		"region":            objCluster,
	}
}

func ciSeedHarborRegistryS3Cmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "seed-harbor-registry-s3",
		Short: "seed secret/harbor/registry-s3 (S3 creds + endpoint derived from the object-storage tfvars)",
		Long: "Native port of the 'Seed Harbor registry S3 credentials in OpenBao'\n" +
			"bootstrap step. Reads HARBOR_REGISTRY_S3_ACCESS_KEY/SECRET_KEY (missing →\n" +
			"::error:: + BOOTSTRAP_ERRORS=true + exit 0 so the remaining seeds run),\n" +
			"resolves obj_cluster from terraform-iac-bootstrap/object-storage/\n" +
			"<region>.tfvars — the source of truth for which Linode OBJ cluster TF\n" +
			"provisioned the bucket into — and writes the access keys + bucket/endpoint/\n" +
			"region in one `kv put`. Reads OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCISeedHarborRegistryS3(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose object-storage tfvars + bucket label to use (required)")
	return c
}

func runCISeedHarborRegistryS3(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	accessKey := os.Getenv("HARBOR_REGISTRY_S3_ACCESS_KEY")
	secretKey := os.Getenv("HARBOR_REGISTRY_S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		if err := appendGHAFile("GITHUB_STEP_SUMMARY",
			"HARBOR_REGISTRY_S3_ACCESS_KEY / HARBOR_REGISTRY_S3_SECRET_KEY not set — skipping secret/harbor/registry-s3.",
			fmt.Sprintf("Add them as infra-%s environment secrets and re-run. Source these from", region),
			`"terraform output -raw harbor_registry_access_key" in the cluster-bootstrap/object-storage workspace.`); err != nil {
			return err
		}
		return flagBootstrapError("HARBOR_REGISTRY_S3_ACCESS_KEY / HARBOR_REGISTRY_S3_SECRET_KEY not set — Harbor registry will CrashLoopBackOff on missing S3 creds")
	}
	maskGHA(accessKey)
	maskGHA(secretKey)

	tfv := filepath.Join("terraform-iac-bootstrap", "object-storage", region+".tfvars")
	content, _ := os.ReadFile(tfv)
	objCluster := tfvarsValue(string(content), "obj_cluster")
	if objCluster == "" {
		fmt.Fprintf(os.Stderr, "::error::obj_cluster not found in %s — cannot resolve the Harbor registry S3 endpoint.\n", tfv)
		return fmt.Errorf("obj_cluster not found in %s", tfv)
	}

	fields := harborRegistryS3Fields(region, objCluster, accessKey, secretKey)
	if err := baoKVPutFn("secret/harbor/registry-s3", fields); err != nil {
		return err
	}
	fmt.Printf("secret/harbor/registry-s3 seeded (bucket=%s, region=%s).\n", fields["bucket_name"], objCluster)
	return nil
}

// ── resolve-harbor-url ────────────────────────────────────────────────────────

func ciResolveHarborURLCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "resolve-harbor-url",
		Short: "default HARBOR_URL to harbor.<domainSuffix> from the LandingZone spec",
		Long: "Native port of the 'Pre-flight — resolve Harbor URL for configuration'\n" +
			"step. HARBOR_URL is the registry hostname buildah pushes to / images pull\n" +
			"from (stored in OpenBao as registry_host) — NOT how the API is reached\n" +
			"(the in-cluster harbor-robot-provisioner talks to harbor-core.harbor.svc).\n" +
			"When the HARBOR_URL env (vars.HARBOR_URL) is set it wins; otherwise\n" +
			"harbor.<domainSuffix> is derived from the LandingZone spec\n" +
			"(spec.environments.<region>.cluster.bootstrap.domainSuffix — the host\n" +
			"apl-core already serves Harbor at) and written to $GITHUB_ENV. This used\n" +
			"to read cluster_domain from the rendered cluster-bootstrap tfvars; the\n" +
			"spec is mandatory now, so that tfvars side-channel (and the cluster_domain\n" +
			"variable it existed for) was retired. Fails only when neither is available.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIResolveHarborURL(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) whose domainSuffix derives the Harbor host (required)")
	return c
}

func runCIResolveHarborURL(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if v := os.Getenv("HARBOR_URL"); v != "" {
		fmt.Printf("HARBOR_URL: %s (from vars.HARBOR_URL).\n", v)
		return nil
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::HARBOR_URL is unset and the LandingZone spec could not be loaded (%v). Set the vars.HARBOR_URL variable, or fix the spec.\n", err)
		return fmt.Errorf("resolve harbor url: %w", err)
	}
	e, ok := lz.Env(region)
	domain := e.Cluster.Bootstrap.DomainSuffix
	if !ok || domain == "" {
		fmt.Fprintf(os.Stderr, "::error::HARBOR_URL is unset and spec.environments.%s.cluster.bootstrap.domainSuffix is empty. Set the vars.HARBOR_URL variable, or fill the spec field.\n", region)
		return fmt.Errorf("domainSuffix not found in the spec for env %s", region)
	}
	fmt.Printf("HARBOR_URL unset — derived harbor.<domainSuffix> = harbor.%s\n", domain)
	return appendGHAFile("GITHUB_ENV", "HARBOR_URL=harbor."+domain)
}

// ── audit-pvc-storageclass ────────────────────────────────────────────────────

// auditWantStorageClass is the encrypted-Retain StorageClass the Kyverno
// mutation rewrites every PVC onto at admission.
const auditWantStorageClass = "block-storage-retain"

// pvcRow is one PVC's identity + StorageClass.
type pvcRow struct {
	Namespace, Name, StorageClass string
}

// parsePVCList extracts pvcRows from `kubectl get pvc -A -o json`. A PVC with
// no storageClassName renders as "<none>", like kubectl custom-columns.
func parsePVCList(out []byte) ([]pvcRow, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				StorageClassName *string `json:"storageClassName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	rows := make([]pvcRow, 0, len(list.Items))
	for _, it := range list.Items {
		sc := "<none>"
		if it.Spec.StorageClassName != nil && *it.Spec.StorageClassName != "" {
			sc = *it.Spec.StorageClassName
		}
		rows = append(rows, pvcRow{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, StorageClass: sc})
	}
	return rows, nil
}

// escapedPVCs filters the PVCs NOT on the wanted StorageClass.
func escapedPVCs(rows []pvcRow, want string) []pvcRow {
	var escaped []pvcRow
	for _, r := range rows {
		if r.StorageClass != want {
			escaped = append(escaped, r)
		}
	}
	return escaped
}

// renderPVCTable renders rows as aligned "NS NAME SC" lines (the
// custom-columns shape the warnings/summary carried).
func renderPVCTable(rows []pvcRow) []string {
	nsW, nameW := 0, 0
	for _, r := range rows {
		nsW, nameW = max(nsW, len(r.Namespace)), max(nameW, len(r.Name))
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%-*s  %-*s  %s", nsW, r.Namespace, nameW, r.Name, r.StorageClass))
	}
	return lines
}

func ciAuditPVCStorageClassCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit-pvc-storageclass",
		Short: "warn about PVCs that escaped the Kyverno encrypted-StorageClass mutation",
		Long: "Native port of the 'Audit PVCs against encrypted-Retain StorageClass'\n" +
			"bootstrap step. The Kyverno mutation rewrites PVCs onto block-storage-retain\n" +
			"at admission, but its webhook has a 30-90s readiness lag after CRD\n" +
			"registration; any PVC apl-core's helmfile created in that window persists\n" +
			"silently on an unencrypted Delete-reclaim class. Lists every such PVC as\n" +
			"::warning:: lines plus a step-summary remediation block. Never fails the\n" +
			"workflow — the cluster is functional, just less secure than intended.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIAuditPVCStorageClass() },
	}
}

func runCIAuditPVCStorageClass() error {
	// kubectl/parse failures read as "no PVCs escaped" — the bash's
	// `2>/dev/null … || true` made this audit best-effort by design.
	var rows []pvcRow
	if out, err := execOutput("kubectl", "get", "pvc", "-A", "-o", "json"); err == nil {
		rows, _ = parsePVCList(out)
	}
	escaped := escapedPVCs(rows, auditWantStorageClass)
	if len(escaped) == 0 {
		fmt.Println("All PVCs are on block-storage-retain — Kyverno admission caught everything.")
		return nil
	}
	table := renderPVCTable(escaped)
	fmt.Fprintf(os.Stderr, "::warning::Found %d PVC(s) NOT on block-storage-retain — Kyverno admission webhook readiness lagged the chart-installed PVC creates.\n", len(escaped))
	for _, l := range table {
		fmt.Fprintf(os.Stderr, "::warning::  %s\n", l)
	}
	summary := append([]string{
		"### PVCs that escaped the Kyverno encryption mutation",
		"",
		"These PVCs landed on a StorageClass other than",
		"`block-storage-retain` because Kyverno's admission",
		"webhook wasn't yet enforcing when apl-core's helmfile created",
		"them. Data is NOT encrypted at rest and reclaim policy is Delete.",
		"",
		"```",
		"NAMESPACE  PVC  STORAGECLASS",
	}, table...)
	summary = append(summary,
		"```",
		"",
		"**To remediate** (per-workload, irreversible for that data):",
		"1. Delete the workload owning the PVC (e.g. `kubectl -n <ns> delete sts <name>`)",
		"2. Delete the PVC (`kubectl -n <ns> delete pvc <name>`)",
		"3. Reapply via Argo sync — new PVC goes through Kyverno admission, lands on the encrypted SC")
	return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
}
