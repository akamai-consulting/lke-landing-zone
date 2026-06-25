package main

// ci_label_volumes.go is `llz ci label-volumes` — the native port of the
// volume-labeler's relabel.sh (formerly a shell script embedded in a ConfigMap,
// which the untestable-loc gate counts as untestable). For every Linode-CSI PV in
// the cluster it relabels the backing Volume to a human-readable
// `<region>-<ns>-<pvc>` and stamps an `lke<cluster_id>` ownership tag so `llz
// reap` can attribute Volumes to their cluster. The DECISIONS (label compute, tag
// merge, PV/node parsing) are pure functions in internal/linode + here; the only
// I/O is `kubectl get` (PVs/nodes) and the Linode Volume API.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// pvVolume is one Linode-CSI PV's backing Volume id + the PVC it's bound to.
type pvVolume struct {
	VolumeID  string
	Namespace string
	PVC       string
}

// parsePVVolumes extracts Linode-CSI PVs that have a bound claim from `kubectl get
// pv -o json`. Mirrors the old jq: csi.driver == linodebs.csi.linode.com,
// claimRef != null; the Volume id is volumeHandle up to the first '-'.
func parsePVVolumes(jsonBytes []byte) ([]pvVolume, error) {
	var doc struct {
		Items []struct {
			Spec struct {
				CSI *struct {
					Driver       string `json:"driver"`
					VolumeHandle string `json:"volumeHandle"`
				} `json:"csi"`
				ClaimRef *struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"claimRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse pv json: %w", err)
	}
	var out []pvVolume
	for _, it := range doc.Items {
		if it.Spec.CSI == nil || it.Spec.CSI.Driver != "linodebs.csi.linode.com" || it.Spec.ClaimRef == nil {
			continue
		}
		id := it.Spec.CSI.VolumeHandle
		if i := strings.IndexByte(id, '-'); i >= 0 {
			id = id[:i]
		}
		if id == "" {
			continue
		}
		out = append(out, pvVolume{VolumeID: id, Namespace: it.Spec.ClaimRef.Namespace, PVC: it.Spec.ClaimRef.Name})
	}
	return out, nil
}

// firstNodeInstanceID returns the Linode instance id of the first node in
// `kubectl get nodes -o json` (via spec.providerID); "" if none resolvable.
func firstNodeInstanceID(jsonBytes []byte) (string, error) {
	var doc struct {
		Items []struct {
			Spec struct {
				ProviderID string `json:"providerID"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return "", fmt.Errorf("parse nodes json: %w", err)
	}
	for _, it := range doc.Items {
		if id := linode.InstanceIDFromProviderID(it.Spec.ProviderID); id != "" {
			return id, nil
		}
	}
	return "", nil
}

// volumeClient is the slice of the Linode client reconcileVolumes needs — seamed
// so the driver is unit-testable with a fake (mirrors ci_preflight's
// orphanScanner).
type volumeClient interface {
	Volume(ctx context.Context, id string) (map[string]any, int, error)
	UpdateVolume(ctx context.Context, id, label string, tags []string) (int, error)
}

type labelVolumesResult struct{ renamed, tagged, ok, missing, errors int }

// reconcileVolumes relabels + cluster-tags each PV's Volume: GET it, compute the
// desired label and merged tag set, and PUT only when something changed. Pure
// except for the injected client; logf receives one line per action.
func reconcileVolumes(ctx context.Context, c volumeClient, regionShort, clusterTag string, pvs []pvVolume, logf func(string, ...any)) labelVolumesResult {
	var r labelVolumesResult
	for _, pv := range pvs {
		vol, status, err := c.Volume(ctx, pv.VolumeID)
		switch {
		case err != nil:
			logf("error %s: GET: %v", pv.VolumeID, err)
			r.errors++
			continue
		case status == 404:
			logf("skip %s: 404 — Linode volume missing", pv.VolumeID)
			r.missing++
			continue
		case status < 200 || status >= 300:
			logf("error %s: GET returned %d", pv.VolumeID, status)
			r.errors++
			continue
		}

		desired := linode.DesiredVolumeLabel(regionShort, pv.Namespace, pv.PVC)
		curLabel := linode.MapString(vol, "label")
		mergedTags, tagChanged := linode.MergeClusterTag(linode.MapTags(vol), clusterTag)
		if curLabel == desired && !tagChanged {
			r.ok++
			continue
		}
		if _, err := c.UpdateVolume(ctx, pv.VolumeID, desired, mergedTags); err != nil {
			logf("error %s: PUT: %v", pv.VolumeID, err)
			r.errors++
			continue
		}
		if curLabel != desired {
			logf("renamed %s: %s -> %s", pv.VolumeID, curLabel, desired)
			r.renamed++
		}
		if tagChanged {
			logf("tagged %s: +%s", pv.VolumeID, clusterTag)
			r.tagged++
		}
	}
	return r
}

func ciLabelVolumesCmd() *cobra.Command {
	var regionShort, clusterID string
	c := &cobra.Command{
		Use:   "label-volumes",
		Short: "relabel + cluster-tag Linode Volumes for the cluster's PVCs",
		Long: "Relabels every Linode-CSI PV's backing Volume to a human-readable\n" +
			"<region>-<namespace>-<pvc> and stamps an lke<cluster_id> ownership tag so\n" +
			"`llz reap` can tell a live cluster's detached PVC from a true orphan. The\n" +
			"cluster id is the --lke-cluster-id flag / LKE_CLUSTER_ID env, else self-\n" +
			"discovered from a node's Linode instance tag. Idempotent; run on a schedule.\n" +
			"Replaces the volume-labeler's embedded relabel.sh.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCILabelVolumes(regionShort, clusterID)
		},
	}
	c.Flags().StringVar(&regionShort, "region-short", os.Getenv("REGION_SHORT"), "region short code for labels, e.g. pri|sec (default env REGION_SHORT)")
	c.Flags().StringVar(&clusterID, "lke-cluster-id", os.Getenv("LKE_CLUSTER_ID"), "override the lke<id> cluster tag (default env LKE_CLUSTER_ID; else self-discovered from a node)")
	return c
}

func runCILabelVolumes(regionShort, clusterID string) error {
	if regionShort == "" {
		return fmt.Errorf("--region-short (or REGION_SHORT) is required")
	}
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_API_TOKEN (or LINODE_TOKEN) to a Linode PAT (read_write)")
	}
	ctx := context.Background()
	client := linode.NewClient(token, 60*time.Second)

	clusterTag, err := resolveClusterTag(ctx, client, clusterID)
	if err != nil {
		return err
	}
	if clusterTag != "" {
		fmt.Printf("cluster tag: %s\n", clusterTag)
	} else {
		fmt.Println("warning: could not resolve LKE cluster id (set LKE_CLUSTER_ID to override); relabeling only, no cluster tag")
	}

	pvJSON, err := execOutput("kubectl", "get", "pv", "-o", "json")
	if err != nil {
		return fmt.Errorf("kubectl get pv: %w", err)
	}
	pvs, err := parsePVVolumes(pvJSON)
	if err != nil {
		return err
	}
	if len(pvs) == 0 {
		fmt.Println("no Linode-CSI PVs with bound PVCs found; nothing to do")
		return nil
	}

	r := reconcileVolumes(ctx, client, regionShort, clusterTag, pvs, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	fmt.Printf("\nsummary: renamed=%d tagged=%d already-ok=%d api-404=%d errors=%d\n", r.renamed, r.tagged, r.ok, r.missing, r.errors)
	if r.errors > 0 {
		return fmt.Errorf("label-volumes: %d error(s)", r.errors)
	}
	return nil
}

// resolveClusterTag returns the `lke<id>` tag for this cluster: the explicit
// override if set, else discovered from the first node's Linode instance tags.
func resolveClusterTag(ctx context.Context, client *linode.Client, clusterID string) (string, error) {
	if clusterID != "" {
		return "lke" + clusterID, nil
	}
	nodesJSON, err := execOutput("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("kubectl get nodes: %w", err)
	}
	iid, err := firstNodeInstanceID(nodesJSON)
	if err != nil {
		return "", err
	}
	if iid == "" {
		return "", nil
	}
	inst, status, err := client.Instance(ctx, iid)
	if err != nil || status < 200 || status >= 300 {
		return "", nil // best-effort: fall back to relabel-only
	}
	return linode.ClusterTagForVolume(linode.MapTags(inst)), nil
}
