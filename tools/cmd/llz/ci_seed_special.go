package main

// ci_seed_special.go implements the one-off seed/verify steps of
// llz-bootstrap-openbao.yml that don't fit the generic `llz ci bao-seed`
// shape (they derive their material instead of just relaying it):
//
//   resolve-harbor-url       default HARBOR_URL to harbor.<domainSuffix> from
//                            the LandingZone spec
//   audit-pvc-storageclass   report PVCs that escaped the Kyverno encrypted-
//                            StorageClass mutation
//
// (seed-harbor-dockerconfig was retired: the harbor docker config.json is now
// derived in-cluster by the llz-cert-automation chart's harborDockerConfig
// ExternalSecret. seed-harbor-registry-s3 was retired too: the object-storage
// keys are no longer TF-minted and GH-relayed — `llz ci mint-bootstrap-objkeys`
// mints and seeds secret/loki/object-store + secret/harbor/registry-s3 in one
// step, and the in-cluster rotator owns them after first boot.)

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
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
	override := os.Getenv("HARBOR_URL")
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		// An override still works without a spec — it just cannot be cross-checked
		// against what the in-cluster provisioner will derive. Only the no-override
		// case is fatal.
		if override != "" {
			fmt.Printf("HARBOR_URL: %s (from vars.HARBOR_URL; spec unreadable, so no cross-check against harbor.<domainSuffix>).\n", override)
			return nil
		}
		fmt.Fprintf(os.Stderr, "::error::HARBOR_URL is unset and the LandingZone spec could not be loaded (%v). Set the vars.HARBOR_URL variable, or fix the spec.\n", err)
		return fmt.Errorf("resolve harbor url: %w", err)
	}
	e, ok := lz.Env(region)
	domain := ""
	if ok {
		domain = e.Cluster.Bootstrap.DomainSuffix
		if domain == "" && e.Cluster.Bootstrap.ManagedAppPlatform {
			// Managed App Platform: Linode owns the lke<id>.akamai-apl.net domain and
			// the spec has no domainSuffix, but managed apl-core serves Harbor at
			// harbor.<managed-domain>. Discover the domain from apl-core in-cluster.
			// Requires cluster access (this preflight runs with the bootstrap
			// kubeconfig); degrades to the HARBOR_URL-override path when unreachable.
			if domain = discoverManagedDomain(); domain != "" {
				fmt.Printf("managed App Platform: discovered domain %s from apl-core in-cluster.\n", domain)
			}
		}
	}
	if !ok || domain == "" {
		if override != "" {
			// An override with no spec to check it against: usable, but say so.
			fmt.Printf("HARBOR_URL: %s (from vars.HARBOR_URL; no domainSuffix in the spec to cross-check).\n", override)
			return nil
		}
		fmt.Fprintf(os.Stderr, "::error::HARBOR_URL is unset and spec.environments.%s.cluster.bootstrap.domainSuffix is empty (and no managed domain could be discovered). Set the vars.HARBOR_URL variable, or fill the spec field.\n", region)
		return fmt.Errorf("domainSuffix not found in the spec for env %s", region)
	}
	derived := clusterspec.HarborHost(domain)

	if override != "" {
		// The in-cluster harbor-robot-provisioner does not read this override, so an
		// override that diverges leaves CI and the provisioner pointed at different
		// registries — the provisioner writes a registry_host CI disagrees with, and
		// nothing reports it. This is what actually checks. Where the provisioner's
		// value COMES from differs by platform, so say which:
		//   - self-install: RenderHarborHostPatch bakes harbor.<domainSuffix>, and
		//     kustomize.go notes the two "must be kept in step".
		//   - managed: HARBOR_HOST renders empty (no domainSuffix), so the provisioner
		//     asks Harbor for its own registry host — ground truth, which an override
		//     cannot move. `derived` above is the same host by a different route (the
		//     discovered apl-core domain), so a mismatch still means the override is
		//     the odd one out.
		if override != derived {
			source := "harbor.<domainSuffix>, from RenderHarborHostPatch"
			if e.Cluster.Bootstrap.ManagedAppPlatform {
				source = "discovered from Harbor's own systeminfo on managed App Platform"
			}
			fmt.Fprintf(os.Stderr, "::warning::HARBOR_URL is %q but the in-cluster provisioner will use %q (%s — it ignores this override). CI and the cluster will disagree about the registry host — align vars.HARBOR_URL with it, or change the domainSuffix.\n", override, derived, source)
		}
		fmt.Printf("HARBOR_URL: %s (from vars.HARBOR_URL).\n", override)
		return nil
	}

	fmt.Printf("HARBOR_URL unset — derived harbor.<domainSuffix> = %s\n", derived)
	return ghaout.Append("GITHUB_ENV", "HARBOR_URL="+derived)
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

// kyvernoScopedNamespaces are the namespaces the pvc-force-encrypted-storage-class
// ClusterPolicy actually matches. MUST stay in sync with the `namespaces:` list in
// manifests/kyverno-pvc-encrypted-storage-class.yaml — TestKyvernoScopeMatchesPolicy
// reads that file and fails if they drift, because a stale list here would go on
// blaming the webhook for PVCs it was never asked to mutate.
var kyvernoScopedNamespaces = []string{"gitea", "istio-system"}

// splitByKyvernoScope partitions escaped PVCs by whether the mutation policy even
// applied to them. The audit used to report every one as "Kyverno webhook
// readiness lagged", which is only true INSIDE that scope; for a harbor or
// keycloak PVC it sends the reader after a timing bug that isn't there. The real
// cause outside the scope is the default-StorageClass ordering — see the step
// summary in runCIAuditPVCStorageClass.
func splitByKyvernoScope(rows []pvcRow) (inScope, outOfScope []pvcRow) {
	scoped := make(map[string]bool, len(kyvernoScopedNamespaces))
	for _, ns := range kyvernoScopedNamespaces {
		scoped[ns] = true
	}
	for _, r := range rows {
		if scoped[r.Namespace] {
			inScope = append(inScope, r)
		} else {
			outOfScope = append(outOfScope, r)
		}
	}
	return inScope, outOfScope
}

func ciAuditPVCStorageClassCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit-pvc-storageclass",
		Short: "warn about PVCs that escaped the Kyverno encrypted-StorageClass mutation",
		Long: "Native port of the 'Audit PVCs against encrypted-Retain StorageClass'\n" +
			"bootstrap step. Lists every PVC not on block-storage-retain as ::warning::\n" +
			"lines plus a step-summary block, SPLIT BY CAUSE. Two different things put a\n" +
			"PVC on an unencrypted Delete-reclaim class:\n" +
			"  • in gitea/istio-system, the Kyverno mutation covers the PVC but its\n" +
			"    webhook has a 30-90s readiness lag after CRD registration, so anything\n" +
			"    apl-core's helmfile created in that window escaped it;\n" +
			"  • anywhere else, Kyverno never applied — the chart honored\n" +
			"    cluster.defaultStorageClass, which defaults to '' (\"use the cluster\n" +
			"    default\"), so the PVC took whatever class was annotated default when\n" +
			"    apl-core created it. Widening the policy would not fix those.\n" +
			"Never fails the workflow — the cluster is functional, just less secure\n" +
			"than intended.",
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
	inScope, outOfScope := splitByKyvernoScope(escaped)
	fmt.Fprintf(os.Stderr, "::warning::Found %d PVC(s) NOT on block-storage-retain (%d in Kyverno's scope, %d never covered by it).\n",
		len(escaped), len(inScope), len(outOfScope))
	for _, l := range table {
		fmt.Fprintf(os.Stderr, "::warning::  %s\n", l)
	}
	summary := append([]string{
		"### PVCs not on the encrypted, Retain StorageClass",
		"",
		"Data on these is NOT encrypted at rest, and their reclaim policy is Delete.",
		"There are TWO distinct causes and only one of them is a Kyverno timing issue —",
		"so read the namespace before concluding anything about webhook readiness.",
		"",
		"```",
		"NAMESPACE  PVC  STORAGECLASS",
	}, table...)
	summary = append(summary,
		"```",
		"",
		fmt.Sprintf("**In `%s` (%d here):** the `pvc-force-encrypted-storage-class`",
			strings.Join(kyvernoScopedNamespaces, "`, `"), len(inScope)),
		"policy DOES cover these, so landing on the wrong class means its admission webhook",
		"was not yet enforcing when apl-core's helmfile created the PVC. Those two charts",
		"(gitea-valkey, oauth2-proxy redis) hardcode `linode-block-storage` on the linode",
		"provider — verified in apl-core v6.0.0 — which is why a mutation is needed at all.",
		"",
		fmt.Sprintf("**Any other namespace (%d here):** NOT a Kyverno problem — the policy is", len(outOfScope)),
		"deliberately scoped to the two namespaces above and never applied to these. Every",
		"other apl-core chart honors `cluster.defaultStorageClass` (verified: harbor.gotmpl,",
		"harbor-otomi-db, keycloak-otomi-db, git-server all template it), and that value",
		"DEFAULTS TO `''` = \"use the cluster's default StorageClass\". So these PVCs took",
		"whichever class was annotated default at the moment apl-core created them. On a",
		"managed (`apl_enabled`) cluster Linode installs apl-core during cluster",
		"provisioning — before `llz ci bootstrap-cluster` promotes block-storage-retain to",
		"default — so LKE's unencrypted class wins the race. Widening the Kyverno policy",
		"would NOT fix this: the PVCs predate the webhook existing.",
		"",
		"**To remediate** (per-workload, irreversible for that data — `storageClassName`",
		"is immutable once bound, so there is no in-place fix):",
		"1. Delete the workload owning the PVC (e.g. `kubectl -n <ns> delete sts <name>`)",
		"2. Delete the PVC (`kubectl -n <ns> delete pvc <name>`)",
		"3. Reapply via Argo sync — by now block-storage-retain is the default and the",
		"   migrated values set `cluster.defaultStorageClass` explicitly, so the new PVC",
		"   lands encrypted whether or not Kyverno covers its namespace.")
	return ghaout.Append("GITHUB_STEP_SUMMARY", summary...)
}
