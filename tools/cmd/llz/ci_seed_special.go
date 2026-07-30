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
	return appendGHAFile("GITHUB_ENV", "HARBOR_URL="+derived)
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
		Short: "FAIL if any PVC is not on the encrypted, ownership-tagged StorageClass",
		Long: "Asserts the storage invariant: every PVC in the cluster is on\n" +
			"block-storage-retain, the only class that encrypts at rest and stamps this\n" +
			"cluster's lke<id> ownership tag at CreateVolume. Lists every escapee as\n" +
			"::warning:: lines plus a step-summary block, then EXITS NON-ZERO.\n" +
			"\n" +
			"This used to warn and always exit 0, on the reasoning that the cluster is\n" +
			"functional and only 'less secure than intended'. That is precisely the class of\n" +
			"defect a gate is for, and exiting 0 is why a regression that put 13 of 16 PVCs\n" +
			"on the unencrypted class shipped unnoticed: the one check that would have\n" +
			"caught it was wired into no workflow AND could not have failed one.\n" +
			"\n" +
			"An escapee is not repairable in place — storageClassName is immutable once\n" +
			"bound — so a red run means re-rolling the workload, not re-running the job.\n" +
			"The step summary spells that out.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIAuditPVCStorageClass() },
	}
}

func runCIAuditPVCStorageClass() error {
	// A kubectl/parse failure is NOT "no PVCs escaped". The bash this was ported
	// from used `2>/dev/null … || true` because it could only warn anyway; now that
	// the verdict gates the build, silently reading an unreachable cluster as a pass
	// would be the worst possible failure mode for a security check.
	out, err := execOutput("kubectl", "get", "pvc", "-A", "-o", "json")
	if err != nil {
		return fmt.Errorf("audit-pvc-storageclass: list PVCs: %w", err)
	}
	rows, err := parsePVCList(out)
	if err != nil {
		return fmt.Errorf("audit-pvc-storageclass: parse PVC list: %w", err)
	}
	escaped := escapedPVCs(rows, auditWantStorageClass)
	if len(escaped) == 0 {
		fmt.Printf("All %d PVC(s) are on %s — encrypted at rest and ownership-tagged.\n", len(rows), auditWantStorageClass)
		return nil
	}
	table := renderPVCTable(escaped)
	fmt.Fprintf(os.Stderr, "::error::Found %d of %d PVC(s) NOT on %s — their data is UNENCRYPTED at rest and their Volumes carry no lke<id> ownership tag.\n",
		len(escaped), len(rows), auditWantStorageClass)
	for _, l := range table {
		fmt.Fprintf(os.Stderr, "::error::  %s\n", l)
	}
	summary := append([]string{
		"### PVCs not on the encrypted, ownership-tagged StorageClass",
		"",
		fmt.Sprintf("Data on these is NOT encrypted at rest, and their Linode Volumes carry no `lke<id>` tag, so `llz ci reap` can neither attribute nor safely sweep them. %d of %d PVCs:", len(escaped), len(rows)),
		"",
		"```",
		"NAMESPACE  PVC  STORAGECLASS",
	}, table...)
	summary = append(summary,
		"```",
		"",
		"**Cause.** A PVC reaches the wrong class exactly one way: it was CREATED naming",
		"an LKE stock class while the `pvc-redirect-untagged-storage-class` ClusterPolicy",
		"was not yet enforcing. On managed, apl-core's `cluster.defaultStorageClass` is",
		"Linode's `linode-block-storage`, so its PVCs name the unencrypted class",
		"EXPLICITLY — promoting `block-storage-retain` to cluster default does not reach",
		"them, because an explicit `storageClassName` never consults the default. So check,",
		"in order: is the policy present (`kubectl get clusterpolicy",
		"pvc-redirect-untagged-storage-class`), is it Ready, and did bootstrap-cluster",
		"apply it before apl-core's helmfile ran?",
		"",
		"**Remediation is per-workload and destroys that volume's data** — `storageClassName`",
		"is immutable once bound, so there is no in-place fix and re-running this job cannot",
		"turn it green:",
		"1. Delete the workload owning the PVC (e.g. `kubectl -n <ns> delete sts <name>`)",
		"2. Delete the PVC (`kubectl -n <ns> delete pvc <name>`)",
		"3. Reapply via Argo sync — with the redirect policy enforcing, the new PVC lands",
		"   on the encrypted, tagged class whatever its chart asks for.")
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", summary...); err != nil {
		return err
	}
	return fmt.Errorf("audit-pvc-storageclass: %d of %d PVC(s) are not on %s (unencrypted at rest, untagged Volumes)",
		len(escaped), len(rows), auditWantStorageClass)
}
