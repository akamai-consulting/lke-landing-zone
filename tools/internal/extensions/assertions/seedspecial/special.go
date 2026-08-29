package seedspecial

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// ── resolve-harbor-url ────────────────────────────────────────────────────────

func RunResolveHarborURL(region string) error {
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
			if domain = identityconfig.DiscoverManagedDomain(); domain != "" {
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
			// The REMEDY has to differ by platform too, because on managed the
			// self-install advice is not merely unhelpful — both halves of it are
			// impossible. "Align vars.HARBOR_URL" cannot be done: the managed host is
			// harbor.lke<id>.akamai-apl.net and the LKE id is new for every cluster, so
			// any fixed value is stale the next time the cluster is rebuilt (e2e proved
			// this — lke648798 one run, lke648821 the next). "Change the domainSuffix"
			// cannot be done either: validateEnv rejects a managed env that sets one
			// ("domainSuffix must NOT be set"). An operator handed two impossible
			// instructions concludes the warning is noise and stops reading it, which
			// is how a real divergence goes unnoticed.
			source := "harbor.<domainSuffix>, from RenderHarborHostPatch"
			remedy := "align vars.HARBOR_URL with it, or change the domainSuffix"
			if e.Cluster.Bootstrap.ManagedAppPlatform {
				source = "discovered from Harbor's own systeminfo on managed App Platform"
				remedy = "UNSET vars.HARBOR_URL and let discovery own it — the managed host embeds the per-cluster LKE id, so no fixed value stays correct"
			}
			fmt.Fprintf(os.Stderr, "::warning::HARBOR_URL is %q but the in-cluster provisioner will use %q (%s — it ignores this override). CI and the cluster will disagree about the registry host — %s.\n", override, derived, source, remedy)
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

// THE KYVERNO SCOPE SPLIT IS GONE, and its absence is the finding.
//
// This audit used to partition escaped PVCs by whether the
// pvc-force-encrypted-storage-class ClusterPolicy covered their namespace, and
// told the reader that an in-scope PVC meant "its admission webhook was not yet
// enforcing when apl-core created the PVC" — a timing bug to go chasing.
//
// That policy has not been applied since LLZ went managed-only: its //go:embed
// went with the self-install flow and was never restored, and the manifest sat in
// bootstrapcluster/manifests/ as an orphan. So the scope was empty, the timing
// story was unreachable, and the success line credited "Kyverno admission caught
// everything" for work Kyverno was not doing. A kyvernoScopedNamespaces list and
// a coupling test kept the two halves of a dead mechanism faithfully in sync.
//
// There is ONE cause now, and it is the one the out-of-scope branch already
// described correctly: the default-StorageClass ordering race.

func RunAuditPVCStorageClass() error {
	// kubectl/parse failures read as "no PVCs escaped" — the bash's
	// `2>/dev/null … || true` made this audit best-effort by design.
	var rows []pvcRow
	if out, err := execOutput("kubectl", "get", "pvc", "-A", "-o", "json"); err == nil {
		rows, _ = parsePVCList(out)
	}
	escaped := escapedPVCs(rows, auditWantStorageClass)
	if len(escaped) == 0 {
		fmt.Println("All PVCs are on block-storage-retain — the canonical encrypted, Retain class.")
		return nil
	}
	table := renderPVCTable(escaped)
	fmt.Fprintf(os.Stderr, "::warning::Found %d PVC(s) NOT on block-storage-retain.\n", len(escaped))
	for _, l := range table {
		fmt.Fprintf(os.Stderr, "::warning::  %s\n", l)
	}
	summary := append([]string{
		"### PVCs not on the encrypted, Retain StorageClass",
		"",
		"These are not on the platform's canonical class, so their reclaim policy is",
		"Delete rather than Retain. Whether their data is ENCRYPTED depends on the class",
		"they did land on: `llz ci bootstrap-cluster` delete+recreates LKE's stock classes",
		"with encryption, so on a cluster bootstrapped since that change they are — check",
		"the storage-class section of `llz ci health`, which reads the live parameters,",
		"rather than assuming either way from this list.",
		"",
		"```",
		"NAMESPACE  PVC  STORAGECLASS",
	}, table...)
	summary = append(summary,
		"```",
		"",
		"**The cause is StorageClass ordering, not admission.** Every apl-core chart that",
		"honors `cluster.defaultStorageClass` defaults it to `''` = \"use the cluster's",
		"default\" (verified: harbor.gotmpl, harbor-otomi-db, keycloak-otomi-db,",
		"git-server), so these PVCs took whichever class was annotated default at the",
		"moment apl-core created them. On a managed cluster Linode installs apl-core during",
		"provisioning — BEFORE `llz ci bootstrap-cluster` promotes block-storage-retain to",
		"default — so LKE's stock class wins the race. Two charts (gitea-valkey,",
		"oauth2-proxy redis) hardcode `linode-block-storage` outright and never consult the",
		"default at all.",
		"",
		"There is no admission policy in this path. An earlier version of this summary",
		"blamed a Kyverno webhook's readiness lag for PVCs in `gitea` and `istio-system`;",
		"that policy has not been applied since LLZ went managed-only, so it could not have",
		"been late — it was absent. The mitigation that DOES run is the class recreate",
		"above, which lands before apl-core creates anything.",
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
