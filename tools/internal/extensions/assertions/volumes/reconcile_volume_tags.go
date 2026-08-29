package volumes

// ci_reconcile_volume_tags.go is `llz ci reconcile-volume-tags` — the in-cluster
// tag-heal backstop, also driven continuously by the llz-reconciler's
// `volume-tags` lane. Primary tagging is the block-storage-retain StorageClass's
// CSI `volumeTags` at CreateVolume; this reconciler exists for the one known path
// that bypasses it — clone/snapshot PVCs admitted while admission control is
// degraded (the Linode CloneVolume API takes no tags and does not copy the
// source's) — plus any future born-untagged path nobody has imagined yet.
//
// One-shot sweep, deliberately narrower than the sibling volume-LABELS lane:
//   - desired tags come from the LIVE StorageClass's volumeTags parameter (the
//     single source of truth `llz ci bootstrap-cluster` renders) — no node-instance
//     lookup, no REGION_SHORT, no per-env config;
//   - tags only, labels untouched — the volume-labels lane owns those;
//   - it also REPORTS (never deletes) this cluster's provably-abandoned Retain
//     Volumes: tagged lke<id> but referenced by no PV — the ClassifyVolume
//     VolKeep blind spot only in-cluster PV knowledge can resolve.
//
// Reads the cluster through the in-process kube client (discoverKubeFn), like the
// sibling reconciler lanes — no kubectl exec.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// linodeCSIDriver identifies the Linode Block Storage CSI driver on a PV's
// spec.csi. Lived beside the volume-labels lane until that lane was retired; it
// is the tag reconciler and the encryption gate that need it now.
const linodeCSIDriver = "linodebs.csi.linode.com"

// volumeTagsSCParam is the CSI parameter key carrying the class's desired tag set.
const volumeTagsSCParam = "linodebs.csi.linode.com/volumeTags"

// DefaultTagsSC is the class bootstrap-cluster renders the lke<id> tag into.
// DefaultTagsSC is the StorageClass whose volumeTags parameter defines the
// required tag set. Moved here with the code that reads it: it is a fact about
// this repo's storage posture, not about any one command's flag default.
const DefaultTagsSC = "block-storage-retain"

// scStorageClassesPath is the API path the in-pod client reads StorageClasses at.
const scStorageClassesPath = "/apis/storage.k8s.io/v1/storageclasses"

// pvVolume is one Linode-CSI PV's backing Volume id (+ bound PVC, for logging).
type pvVolume struct {
	VolumeID string
	// HandleLabel is the LABEL half of the CSI volumeHandle (`<id>-<label>`), i.e.
	// the label the volume carried at CreateVolume. The Linode CSI builds the
	// device path it looks for from THIS string (findDevicePath ->
	// GetNormalizedLabel -> GetDiskByIdPaths), and the handle is immutable — so it
	// is the only label under which the volume can ever be mounted again. Captured
	// so a gate can compare it against the volume's LIVE label; they diverge
	// exactly when something renamed the volume, which is unmountable.
	HandleLabel string
	Namespace   string
	PVC         string
	// Phase is the PV's status.phase. Only the LABEL expectation depends on it:
	// labels are account-unique so a Released PV sharing a claimRef with a live one
	// can never take the derived name, while tags and encryption remain meaningful
	// on any phase.
	Phase string
}

// parsePVVolumes extracts the backing Linode Volume of every Linode-CSI PV in a
// PersistentVolume list. Unlike the volume-labels lane it does NOT require a bound
// claimRef — a released PV's Volume still needs its ownership tag. The Volume id is
// the leading segment of the CSI volumeHandle (`<id>-<name>` on LKE-E).
func parsePVVolumes(pvList map[string]any) []pvVolume {
	items, _ := pvList["items"].([]any)
	var out []pvVolume
	for _, it := range items {
		pv, ok := it.(map[string]any)
		if !ok {
			continue
		}
		spec, _ := pv["spec"].(map[string]any)
		csi, _ := spec["csi"].(map[string]any)
		if csi == nil || csi["driver"] != linodeCSIDriver {
			continue
		}
		id, _ := csi["volumeHandle"].(string)
		handleLabel := ""
		if i := strings.IndexByte(id, '-'); i >= 0 {
			handleLabel = id[i+1:]
			id = id[:i]
		}
		if id == "" {
			continue
		}
		v := pvVolume{VolumeID: id, HandleLabel: handleLabel}
		if st, ok := pv["status"].(map[string]any); ok {
			v.Phase, _ = st["phase"].(string)
		}
		if claim, ok := spec["claimRef"].(map[string]any); ok {
			v.Namespace, _ = claim["namespace"].(string)
			v.PVC, _ = claim["name"].(string)
		}
		out = append(out, v)
	}
	return out
}

// desiredTagsFromSC extracts the CSV volumeTags parameter from a StorageClass — the
// reconciler's desired set. An empty/missing parameter is an error: it means the
// class itself is broken (the health check hard-fails on it too), and healing to an
// empty set would be a destructive no-op.
func desiredTagsFromSC(sc map[string]any, scName string) ([]string, error) {
	params, _ := sc["parameters"].(map[string]any)
	raw, _ := params[volumeTagsSCParam].(string)
	var tags []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("StorageClass %s has no %s — refusing to reconcile to an empty tag set (fix the class; `llz ci health` flags this)", scName, volumeTagsSCParam)
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

// tagReconcileLinodeFn opens the Linode client. Seamed for tests.
var tagReconcileLinodeFn = func(token string) tagReconcileClient {
	return capability.CloudFor(cloudBinding("volume-tags")).Client(token, 60*time.Second)
}

// maxLinodeTag is Linode's tag-length cap (the API declares minLength 3,
// maxLength 50). Distinct from the 32-char Volume LABEL cap, and the extra
// headroom is why <namespace>-<pvc> fits here where it did not always fit there.
const maxLinodeTag = 50

// workloadTags are the per-Volume tags carrying what the retired volume-labels
// lane used to put in the LABEL: which workload owns this Volume.
//
// TAGS, NOT THE LABEL, AND THAT IS THE WHOLE POINT. The label is load-bearing —
// the CSI resolves the block device from the label baked into the PV's immutable
// volumeHandle, so writing it after creation permanently breaks the next mount
// (see the retirement of relabel-volumes). NOTHING reads tags to find a device,
// so they can be written, corrected and rewritten freely. This restores the
// readability the label used to give without any of the risk.
//
// Two tags rather than one joined string, because they answer different
// questions in the Cloud Manager: `ns-<namespace>` filters every Volume in a
// namespace (few distinct values, one per namespace), and `<namespace>-<pvc>`
// identifies the single workload. Unlike labels, tags need not be
// account-unique, so StatefulSet replicas do NOT collide here — the failure that
// made the old relabeler reject 17 of 17 renames cannot happen.
//
// NO REGION_SHORT, deliberately, even though the old label began with it. This
// lane is documented as taking no per-env config, and `lke<id>` from the
// StorageClass already identifies the cluster more precisely than a 3-letter
// prefix did.
func workloadTags(pv pvVolume) []string {
	if pv.Namespace == "" || pv.PVC == "" {
		return nil // a released PV with no claim has no workload to name
	}
	return []string{
		fitLinodeTag("ns-" + pv.Namespace),
		fitLinodeTag(pv.Namespace + "-" + pv.PVC),
	}
}

// fitLinodeTag sanitizes to the charset Linode accepted for Volume labels
// ([A-Za-z0-9_-], the safe subset) and squeezes to maxLinodeTag by dropping from
// the MIDDLE, keeping the tail.
//
// Keeping the tail is not cosmetic. A StatefulSet's ordinal lives on the right,
// so truncating from the right collapses `data-platform-openbao-{0,1,2}` into one
// string — which is exactly how the old relabeler made three replicas ask for a
// single account-unique label and fail. Tags tolerate duplicates, so this is a
// readability bug here rather than a breakage, but it is the same mistake and it
// is just as easy to avoid.
func fitLinodeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > maxLinodeTag {
		const tailKeep = 8
		head := maxLinodeTag - tailKeep - 1 // -1 for the joining '-'
		tail := strings.TrimLeft(out[len(out)-tailKeep:], "-")
		out = strings.TrimRight(out[:head], "-") + "-" + tail
	}
	return strings.TrimRight(out, "-")
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
		// Per-Volume tags on top of the class's static set. A fresh slice: append
		// onto `desired` would scribble into the caller's backing array and leak
		// one Volume's workload tags onto the next.
		want := make([]string, 0, len(desired)+2)
		want = append(want, desired...)
		want = append(want, workloadTags(pv)...)
		merged, changed := linode.MergeTags(linode.MapTags(vol), want)
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
//
// Deliberately NOT filtered on a `pvc-` label prefix, unlike reap's scope filter:
// the sibling volume-LABELS lane renames every bound Volume to
// `<region>-<ns>-<pvc>`, so on any cluster running that lane a prefix filter would
// hide exactly the Volumes this report exists to surface. The lke<id> tag plus "no
// PV references it" is the sound signal, and it is label-independent. The cost is a
// false positive for a hand-created Volume an operator tagged lke<id> themselves —
// acceptable in a report that never deletes anything.
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
		if inPV[id] {
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
		logf("abandoned %s (%s): tagged %s but no PV references it — accruing cost; reclaim deliberately with `llz --yes ci reap-volumes --volume-ids %s`",
			id, linode.MapString(v, "label"), lkeTag, id)
		abandoned++
	}
	return abandoned, nil
}

func ReconcileTags(ctx context.Context, d Deps, scName string) error {
	token := d.Token
	if token == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (env or the optional linode-api-token Secret volume)")
	}
	if scName == "" {
		scName = DefaultTagsSC
	}

	k := d.Kube
	sc, status, err := k.GetJSON(ctx, scStorageClassesPath+"/"+scName)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || sc == nil {
		return fmt.Errorf("GET storageclass %s: status %d", scName, status)
	}
	desired, err := desiredTagsFromSC(sc, scName)
	if err != nil {
		return err
	}
	fmt.Printf("desired tags (from StorageClass %s): %s\n", scName, strings.Join(desired, ","))

	pvList, status, err := k.GetJSON(ctx, "/api/v1/persistentvolumes")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || pvList == nil {
		return fmt.Errorf("GET persistentvolumes: status %d", status)
	}
	pvs := parsePVVolumes(pvList)

	client := tagReconcileLinodeFn(token)
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
