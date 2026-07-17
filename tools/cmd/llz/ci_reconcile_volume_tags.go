package main

// ci_reconcile_volume_tags.go is `llz ci reconcile-volume-tags` — the in-cluster
// tag-heal backstop (volumeTagReconciler CronJob). Primary tagging is the
// block-storage-retain StorageClass's CSI `volumeTags` at CreateVolume; this
// reconciler exists for the one known path that bypasses it — clone/snapshot
// PVCs admitted while admission control is degraded (the Linode CloneVolume API
// takes no tags and does not copy the source's) — plus any future
// born-untagged path nobody has imagined yet.
//
// One-shot sweep, deliberately narrower than the retired volume-labeler:
//   - desired tags come from the LIVE StorageClass's volumeTags parameter (the
//     single source of truth cluster-bootstrap renders) — no node-instance
//     lookup, no REGION_SHORT, no per-env config;
//   - tags only, labels untouched — Volumes keep `pvc-<uuid>`; renaming them
//     would break `llz reap`'s `pvc-` candidate filter;
//   - it also REPORTS (never deletes) this cluster's provably-abandoned Retain
//     Volumes: tagged lke<id> but referenced by no PV — the ClassifyVolume
//     VolKeep blind spot only in-cluster PV knowledge can resolve.

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

// pvVolume is one Linode-CSI PV's backing Volume id (+ bound PVC, for logging).
type pvVolume struct {
	VolumeID  string
	Namespace string
	PVC       string
}

// pvObject is the slice of a PersistentVolume the reconciler reads: the CSI
// driver + volume handle, and the bound claim when present (logging only —
// unlike the retired labeler, an unbound/released PV is still healed).
type pvObject struct {
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
}

// toPVVolume maps a PV to its backing Linode Volume; ok=false unless it's a
// Linode-CSI PV with a parseable id (volumeHandle up to the first '-').
func (p pvObject) toPVVolume() (pvVolume, bool) {
	if p.Spec.CSI == nil || p.Spec.CSI.Driver != "linodebs.csi.linode.com" {
		return pvVolume{}, false
	}
	id := p.Spec.CSI.VolumeHandle
	if i := strings.IndexByte(id, '-'); i >= 0 {
		id = id[:i]
	}
	if id == "" {
		return pvVolume{}, false
	}
	pv := pvVolume{VolumeID: id}
	if p.Spec.ClaimRef != nil {
		pv.Namespace, pv.PVC = p.Spec.ClaimRef.Namespace, p.Spec.ClaimRef.Name
	}
	return pv, true
}

// parsePVVolumes extracts Linode-CSI PVs from `kubectl get pv -o json`.
func parsePVVolumes(jsonBytes []byte) ([]pvVolume, error) {
	var doc struct {
		Items []pvObject `json:"items"`
	}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return nil, fmt.Errorf("parse pv json: %w", err)
	}
	var out []pvVolume
	for _, it := range doc.Items {
		if pv, ok := it.toPVVolume(); ok {
			out = append(out, pv)
		}
	}
	return out, nil
}

// desiredTagsFromSC extracts the CSV volumeTags parameter from a StorageClass
// JSON document — the reconciler's desired set. An empty/missing parameter is an
// error: it means the class itself is broken (the health check hard-fails on it
// too), and healing to an empty set would be a destructive no-op.
func desiredTagsFromSC(scJSON []byte, scName string) ([]string, error) {
	var sc struct {
		Parameters map[string]string `json:"parameters"`
	}
	if err := json.Unmarshal(scJSON, &sc); err != nil {
		return nil, fmt.Errorf("parse storageclass json: %w", err)
	}
	var tags []string
	for _, t := range strings.Split(sc.Parameters["linodebs.csi.linode.com/volumeTags"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("StorageClass %s has no linodebs.csi.linode.com/volumeTags — refusing to reconcile to an empty tag set (fix the class; `llz ci health` flags this)", scName)
	}
	return tags, nil
}

// tagReconcileClient is the slice of the Linode client the reconciler needs —
// seamed so the driver is unit-testable with a fake.
type tagReconcileClient interface {
	Volume(ctx context.Context, id string) (map[string]any, int, error)
	UpdateVolume(ctx context.Context, id, label string, tags []string) (int, error)
	ListVolumes(ctx context.Context) ([]map[string]any, error)
}

type reconcileTagsResult struct{ healed, ok, missing, errors int }

// reconcileVolumeTags heals each PV-backed Volume to carry every desired tag:
// GET it, merge, PUT only when something is missing. Labels pass through
// unchanged. logf receives one line per action.
func reconcileVolumeTags(ctx context.Context, c tagReconcileClient, desired []string, pvs []pvVolume, logf func(string, ...any)) reconcileTagsResult {
	var r reconcileTagsResult
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
		merged, changed := linode.MergeTags(linode.MapTags(vol), desired)
		if !changed {
			r.ok++
			continue
		}
		if _, err := c.UpdateVolume(ctx, pv.VolumeID, linode.MapString(vol, "label"), merged); err != nil {
			logf("error %s: PUT: %v", pv.VolumeID, err)
			r.errors++
			continue
		}
		logf("healed %s (%s/%s): tags -> %s", pv.VolumeID, pv.Namespace, pv.PVC, strings.Join(merged, ","))
		r.healed++
	}
	return r
}

// reportAbandonedVolumes lists Volumes carrying this cluster's lke<id> tag that
// no PV references — provably-abandoned Retain Volumes (their PV is gone, so
// nothing in Kubernetes can ever remount them). REPORT ONLY: account-level reap
// keeps them (VolKeep — cluster is live); reclaiming is a deliberate operator
// action via `llz ci reap-volumes --volume-ids`. Returns the abandoned count.
func reportAbandonedVolumes(ctx context.Context, c tagReconcileClient, lkeTag string, pvs []pvVolume, logf func(string, ...any)) (int, error) {
	inPV := make(map[string]bool, len(pvs))
	for _, pv := range pvs {
		inPV[pv.VolumeID] = true
	}
	vols, err := c.ListVolumes(ctx)
	if err != nil {
		return 0, fmt.Errorf("list Volumes: %w", err)
	}
	abandoned := 0
	for _, v := range vols {
		id := linode.MapIDString(v)
		label := linode.MapString(v, "label")
		if !strings.HasPrefix(label, "pvc-") || inPV[id] {
			continue
		}
		has := false
		for _, t := range linode.MapTags(v) {
			if t == lkeTag {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		logf("abandoned %s (%s): tagged %s but no PV references it — accruing cost; reclaim deliberately with `llz --yes ci reap-volumes --volume-ids %s`", id, label, lkeTag, id)
		abandoned++
	}
	return abandoned, nil
}

func ciReconcileVolumeTagsCmd() *cobra.Command {
	var scName string
	c := &cobra.Command{
		Use:   "reconcile-volume-tags",
		Short: "heal StorageClass volumeTags onto every PV-backed Linode Volume",
		Long: "One-shot tag reconciler (the volumeTagReconciler CronJob): reads the desired\n" +
			"tag set from the StorageClass's linodebs.csi.linode.com/volumeTags parameter,\n" +
			"lists the cluster's Linode-CSI PVs, and PUTs any missing tags onto their\n" +
			"backing Volumes (labels untouched). Exists for Volumes born untagged — e.g. a\n" +
			"clone/snapshot PVC admitted while admission control was degraded (the Linode\n" +
			"clone API takes no tags). Also reports (never deletes) this cluster's\n" +
			"abandoned Retain Volumes: tagged lke<id> but referenced by no PV.\n" +
			"Reads LINODE_TOKEN (or LINODE_API_TOKEN); needs kubectl access to PVs + the\n" +
			"StorageClass. Idempotent.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIReconcileVolumeTags(scName) },
	}
	c.Flags().StringVar(&scName, "storage-class", "block-storage-retain", "StorageClass whose volumeTags parameter defines the desired tag set")
	return c
}

func runCIReconcileVolumeTags(scName string) error {
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_API_TOKEN (or LINODE_TOKEN) to a Linode PAT (read_write)")
	}
	scJSON, err := execOutput("kubectl", "get", "storageclass", scName, "-o", "json")
	if err != nil {
		return fmt.Errorf("kubectl get storageclass %s: %w", scName, err)
	}
	desired, err := desiredTagsFromSC(scJSON, scName)
	if err != nil {
		return err
	}
	fmt.Printf("desired tags (from StorageClass %s): %s\n", scName, strings.Join(desired, ","))

	pvJSON, err := execOutput("kubectl", "get", "pv", "-o", "json")
	if err != nil {
		return fmt.Errorf("kubectl get pv: %w", err)
	}
	pvs, err := parsePVVolumes(pvJSON)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := linode.NewClient(token, 60*time.Second)
	r := reconcileVolumeTags(ctx, client, desired, pvs, logfLine)

	// Abandoned-Retain report, keyed on the lke<id> tag in the desired set. A
	// class without one (health-check FAIL state) just skips the report.
	abandoned := 0
	if id := linode.LKEIDFromTags(desired); id != "" {
		if abandoned, err = reportAbandonedVolumes(ctx, client, "lke"+id, pvs, logfLine); err != nil {
			logfLine("warning: abandoned-volume report failed: %v", err)
		}
	}

	fmt.Printf("\nsummary: healed=%d already-ok=%d api-404=%d errors=%d abandoned-reported=%d\n",
		r.healed, r.ok, r.missing, r.errors, abandoned)
	if r.errors > 0 {
		return fmt.Errorf("reconcile-volume-tags: %d error(s)", r.errors)
	}
	return nil
}

func logfLine(f string, a ...any) { fmt.Printf(f+"\n", a...) }
