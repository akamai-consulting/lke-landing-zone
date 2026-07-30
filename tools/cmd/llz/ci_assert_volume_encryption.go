package main

// ci_assert_volume_encryption.go is `llz ci assert-volume-encryption` — the e2e
// gate for the storage invariant: every Linode Volume backing a PV in this cluster
// is ENCRYPTED at rest and carries this cluster's `lke<id>` ownership tag.
//
// It asserts against the LINODE API, not against Kubernetes. That is the whole
// point. The obvious cheaper check — "is every PVC on block-storage-retain?" — is a
// PROXY: it infers encryption from the name of the class the PVC asked for. That
// proxy is exactly what was in place while 13 of 16 PVCs on lke637888 provisioned
// unencrypted, and it can be satisfied by a class whose parameters say nothing
// about encryption, or defeated by a class that encrypts under a different name.
// `encryption: "enabled"` on the Volume object is the ground truth, and it is the
// only thing that survives someone renaming or re-parameterising a StorageClass.
//
// FAIL-CLOSED on every ambiguity: an unreachable cluster, an unreachable Linode
// API, a Volume the API will not return, and — deliberately — a cluster with NO
// Linode-CSI PVs at all. A security gate that reports "nothing wrong" when it
// examined nothing is worse than no gate, because it launders an absence of
// evidence into a green check.
//
// Encryption is decided inside CreateVolume and `storageClassName` is immutable
// once bound, so a red run here is NOT re-runnable: it means the workload must be
// re-rolled onto a class that encrypts. The output says so.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// volumeEncryptionEnabled is the Linode API's value for an encrypted Volume.
// The field is absent on volumes predating the feature, which reads as NOT
// encrypted — the correct bias.
const volumeEncryptionEnabled = "enabled"

// volumeVerdict is one PV-backed Volume's compliance with the storage invariant.
type volumeVerdict struct {
	pvVolume
	Label       string
	Encryption  string
	MissingTags []string
	// Unreachable records why the Volume could not be judged. A Volume we cannot
	// read is a FAILURE, never a pass — see the fail-closed note in the header.
	Unreachable string
}

func (v volumeVerdict) ok() bool {
	return v.Unreachable == "" && v.Encryption == volumeEncryptionEnabled && len(v.MissingTags) == 0
}

// problem renders the single most important thing wrong with this Volume.
func (v volumeVerdict) problem() string {
	switch {
	case v.Unreachable != "":
		return "UNREADABLE: " + v.Unreachable
	case v.Encryption != volumeEncryptionEnabled:
		enc := v.Encryption
		if enc == "" {
			enc = "<unset>"
		}
		return "NOT ENCRYPTED (encryption=" + enc + ")"
	default:
		return "untagged: missing " + strings.Join(v.MissingTags, ",")
	}
}

// judgeVolume compares one fetched Volume against the invariant. Pure, so the
// classification is testable without a Linode account.
func judgeVolume(pv pvVolume, vol map[string]any, desired []string) volumeVerdict {
	v := volumeVerdict{pvVolume: pv}
	if vol == nil {
		v.Unreachable = "Linode API returned no volume"
		return v
	}
	v.Label = linode.MapString(vol, "label")
	v.Encryption = linode.MapString(vol, "encryption")

	have := make(map[string]bool)
	for _, t := range linode.MapTags(vol) {
		have[t] = true
	}
	for _, want := range desired {
		if !have[want] {
			v.MissingTags = append(v.MissingTags, want)
		}
	}
	sort.Strings(v.MissingTags)
	return v
}

func ciAssertVolumeEncryptionCmd() *cobra.Command {
	var scName string
	c := &cobra.Command{
		Use:   "assert-volume-encryption",
		Short: "FAIL if any PV-backed Linode Volume is unencrypted or missing its lke<id> tag",
		Long: "E2E gate for the storage invariant. Lists every Linode-CSI PV in the cluster,\n" +
			"GETs its backing Volume from the Linode API, and fails unless EVERY one reports\n" +
			"encryption=enabled and carries the tag set the StorageClass defines.\n" +
			"\n" +
			"Checks the Linode API rather than the PVC's storageClassName on purpose: the\n" +
			"class name is a proxy for encryption, and it was a satisfied proxy the whole\n" +
			"time a managed cluster was provisioning unencrypted Volumes. `encryption` on\n" +
			"the Volume itself is the fact.\n" +
			"\n" +
			"Fails closed on every ambiguity, INCLUDING a cluster with no Linode-CSI PVs —\n" +
			"a gate that passes having examined nothing is worse than no gate.\n" +
			"\n" +
			"A red run is not re-runnable: encryption is set inside CreateVolume and\n" +
			"storageClassName is immutable once bound, so the fix is to re-roll the workload\n" +
			"onto a class that encrypts (which destroys that volume's data).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCIAssertVolumeEncryption(cmd.Context(), scName)
		},
	}
	c.Flags().StringVar(&scName, "storage-class", defaultVolumeTagsSC,
		"StorageClass whose volumeTags parameter defines the required tag set")
	return c
}

func runCIAssertVolumeEncryption(ctx context.Context, scName string) error {
	token := inclusterLinodeToken()
	if token == "" {
		return fmt.Errorf("assert-volume-encryption: LINODE_TOKEN must be set (env or the optional linode-api-token Secret volume) — without it this check cannot read Volume encryption state, and skipping it would be a silent pass")
	}
	if scName == "" {
		scName = defaultVolumeTagsSC
	}

	// Read the cluster through kubectl, NOT the in-pod client the sibling
	// reconcile-volume-tags lane uses: this runs on a CI runner against a fetched
	// kubeconfig, like every other `llz ci assert-*` in the e2e suite. discoverKubeFn
	// is in-cluster only and would fail here.
	scRaw, err := execOutput("kubectl", "get", "storageclass", scName, "-o", "json")
	if err != nil {
		return fmt.Errorf("assert-volume-encryption: get storageclass %s: %w", scName, err)
	}
	var sc map[string]any
	if err := json.Unmarshal(scRaw, &sc); err != nil {
		return fmt.Errorf("assert-volume-encryption: parse storageclass %s: %w", scName, err)
	}
	desired, err := desiredTagsFromSC(sc, scName)
	if err != nil {
		return err
	}

	pvRaw, err := execOutput("kubectl", "get", "persistentvolumes", "-o", "json")
	if err != nil {
		return fmt.Errorf("assert-volume-encryption: list persistentvolumes: %w", err)
	}
	var pvList map[string]any
	if err := json.Unmarshal(pvRaw, &pvList); err != nil {
		return fmt.Errorf("assert-volume-encryption: parse persistentvolumes: %w", err)
	}
	pvs := parsePVVolumes(pvList)
	if len(pvs) == 0 {
		return fmt.Errorf("assert-volume-encryption: found NO Linode-CSI PersistentVolumes. On a converged cluster the platform always has some (openbao, harbor, keycloak, monitoring), so this means the cluster is not converged or PV discovery broke — either way there is nothing to attest and a pass here would be vacuous")
	}

	client := tagReconcileLinodeFn(token)
	verdicts := make([]volumeVerdict, 0, len(pvs))
	for _, pv := range pvs {
		vol, status, err := client.Volume(ctx, pv.VolumeID)
		switch {
		case err != nil:
			verdicts = append(verdicts, volumeVerdict{pvVolume: pv, Unreachable: fmt.Sprintf("GET: %v", err)})
			continue
		case status < 200 || status >= 300:
			verdicts = append(verdicts, volumeVerdict{pvVolume: pv, Unreachable: fmt.Sprintf("GET returned %d", status)})
			continue
		}
		verdicts = append(verdicts, judgeVolume(pv, vol, desired))
	}

	return reportVolumeEncryption(verdicts, desired, scName)
}

// reportVolumeEncryption prints the verdict and returns the pass/fail. Split out so
// the reporting is exercised by tests without a cluster or a Linode account.
func reportVolumeEncryption(verdicts []volumeVerdict, desired []string, scName string) error {
	var bad []volumeVerdict
	for _, v := range verdicts {
		if !v.ok() {
			bad = append(bad, v)
		}
	}
	if len(bad) == 0 {
		fmt.Printf("All %d PV-backed Linode Volume(s) are encrypted at rest and carry %s.\n",
			len(verdicts), strings.Join(desired, ","))
		return nil
	}

	fmt.Printf("::error::%d of %d PV-backed Linode Volume(s) violate the storage invariant (required tags from StorageClass %s: %s)\n",
		len(bad), len(verdicts), scName, strings.Join(desired, ","))
	lines := make([]string, 0, len(bad))
	for _, v := range bad {
		where := v.Namespace + "/" + v.PVC
		if v.Namespace == "" && v.PVC == "" {
			where = "<released PV, no claimRef>"
		}
		line := fmt.Sprintf("volume %s (%s) %s — %s", v.VolumeID, v.Label, where, v.problem())
		lines = append(lines, line)
		fmt.Printf("::error::  %s\n", line)
	}

	summary := append([]string{
		"### Linode Volumes violating the storage invariant",
		"",
		fmt.Sprintf("%d of %d PV-backed Volumes are unencrypted, untagged, or unreadable.", len(bad), len(verdicts)),
		"",
		"```",
	}, lines...)
	summary = append(summary,
		"```",
		"",
		"**Encryption is not repairable in place.** It is decided inside CreateVolume and",
		"`storageClassName` is immutable once bound, so re-running this job cannot turn it",
		"green. Remediation is per-workload and destroys that volume's data: delete the",
		"owning workload, delete the PVC, re-sync so it is recreated on a class that",
		"encrypts.",
		"",
		"**If these landed on an LKE stock class** (`linode-block-storage[-retain]`), the",
		"cause is upstream of the PVC: on managed, apl-core's `cluster.defaultStorageClass`",
		"is Linode's unencrypted class and its PVCs name it explicitly. Check that",
		"`llz ci bootstrap-cluster` recreated the stock classes encrypted, and that LKE has",
		"not re-promoted its own unencrypted definitions since.")
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", summary...); err != nil {
		return err
	}
	return fmt.Errorf("assert-volume-encryption: %d of %d PV-backed Linode Volume(s) are not encrypted-and-tagged", len(bad), len(verdicts))
}
