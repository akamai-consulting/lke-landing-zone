package volumes

// ci_relabel_volumes.go implements `llz ci relabel-volumes` — the Go port of the
// linode-volume-labeler `relabel.sh` CronJob script. For every bound Linode-CSI
// PV in the cluster it rewrites the backing Linode Volume's UI label from the
// CSI default (`pvc-<uuid>` on LKE-E, because the managed CSI controller's
// --volume-label-prefix is empty) to a human-readable
// `<REGION_SHORT>-<namespace>-<pvc-name>`, sanitized to Linode's charset and
// truncated to the 32-char label cap. Idempotent and rate-limited-friendly:
// already-correct labels are skipped. It lists all account Volumes ONCE (vs the
// script's per-volume GET) and matches by id.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

const (
	linodeCSIDriver = "linodebs.csi.linode.com"
	maxLinodeLabel  = 32
)

// volumeLabeler is the slice of the Linode client the relabeler needs; seamed so
// tests drive the reconcile without a live account.
type volumeLabeler interface {
	ListVolumes(ctx context.Context) ([]map[string]any, error)
	UpdateVolumeLabel(ctx context.Context, id uint64, label string) error
}

// relabelLinodeFn opens the Linode client. Seamed for tests.
var relabelLinodeFn = func(token string) volumeLabeler { return linode.NewClient(token, 30*time.Second) }

func Relabel(ctx context.Context, d Deps) error {
	regionShort := os.Getenv("REGION_SHORT")
	if regionShort == "" {
		return fmt.Errorf("REGION_SHORT must be set (e.g. pri|sec|sta|lab)")
	}
	token := d.Token
	if token == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (env or the optional linode-api-token Secret volume)")
	}
	k := d.Kube
	pvList, status, err := k.GetJSON(ctx, "/api/v1/persistentvolumes")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || pvList == nil {
		return fmt.Errorf("GET persistentvolumes: status %d", status)
	}

	vols := linodeCSIVolumes(pvList)
	if len(vols) == 0 {
		fmt.Println("no Linode-CSI PVs with bound PVCs found; nothing to relabel")
		return nil
	}

	lc := relabelLinodeFn(token)
	all, err := lc.ListVolumes(ctx)
	if err != nil {
		return err
	}
	labelByID := volumeLabelsByID(all)

	var renamed, alreadyOK, missing, errs int
	for _, v := range vols {
		desired := desiredVolumeLabel(regionShort, v.namespace, v.pvcName)
		cur, present := labelByID[v.id]
		switch {
		case !present:
			missing++ // volume deleted out-of-band while the PV still references it
		case cur == desired:
			alreadyOK++
		default:
			if err := lc.UpdateVolumeLabel(ctx, v.id, desired); err != nil {
				fmt.Fprintf(os.Stderr, "error relabeling volume %d: %v\n", v.id, err)
				errs++
				continue
			}
			fmt.Printf("renamed %d: %s -> %s\n", v.id, cur, desired)
			renamed++
		}
	}
	fmt.Printf("summary: renamed=%d already-ok=%d missing=%d errors=%d\n", renamed, alreadyOK, missing, errs)
	if errs > 0 {
		return fmt.Errorf("%d volume relabel error(s)", errs)
	}
	return nil
}

// csiVolume is one bound Linode-CSI PV: the Linode Volume id + its claim's
// namespace/pvc-name (the inputs to the desired label).
type csiVolume struct {
	id        uint64
	namespace string
	pvcName   string
}

// linodeCSIVolumes extracts a csiVolume for every BOUND PV backed by the Linode
// CSI driver. The Volume id is the leading segment of the CSI volumeHandle
// (`<id>-<name>` on LKE-E).
//
// PHASE MATTERS, and checking claimRef alone is not enough. A RELEASED PV keeps
// its claimRef — that is precisely what makes it Released rather than Available —
// so a claimRef test admits every leaked PV from every previous incarnation of a
// StatefulSet pod. With `Retain`, `monitoring/storage-loki-0` accumulates one per
// cluster rebuild.
//
// That matters because desiredVolumeLabel is a pure function of
// (region, namespace, pvc): every one of those Released PVs wants the SAME label,
// and Linode Volume labels are ACCOUNT-UNIQUE. So the first rename wins and every
// other one fails with a duplicate-label error, forever. The volumes keep their
// pvc-<uuid> names, assert-volume-encryption reports them as un-relabeled, and no
// amount of waiting helps — it is not a slow lane, it is an impossible one.
//
// Skipping non-Bound PVs does NOT make them unreapable: reap accepts the `pvc-`
// prefix (VolumeLabelPrefixes), which is exactly what they keep.
func linodeCSIVolumes(pvList map[string]any) []csiVolume {
	items, _ := pvList["items"].([]any)
	var out []csiVolume
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
		claim, _ := spec["claimRef"].(map[string]any)
		if claim == nil {
			continue
		}
		// Bound only — see the phase note above.
		if status, _ := pv["status"].(map[string]any); status != nil {
			if phase, _ := status["phase"].(string); phase != "" && phase != "Bound" {
				continue
			}
		}
		handle, _ := csi["volumeHandle"].(string)
		idStr := handle
		if i := strings.IndexByte(handle, '-'); i >= 0 {
			idStr = handle[:i]
		}
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}
		ns, _ := claim["namespace"].(string)
		name, _ := claim["name"].(string)
		out = append(out, csiVolume{id: id, namespace: ns, pvcName: name})
	}
	return out
}

// volumeLabelsByID indexes an account Volume list by id → current label.
func volumeLabelsByID(vols []map[string]any) map[uint64]string {
	m := make(map[uint64]string, len(vols))
	for _, v := range vols {
		id := linode.MapUint(v, "id")
		if id == 0 {
			continue
		}
		label, _ := v["label"].(string)
		m[id] = label
	}
	return m
}

// desiredVolumeLabel builds the target Linode label: <region>-<namespace>-<pvc>,
// mapping every char outside Linode's [A-Za-z0-9_-] set to '-', truncating to the
// 32-char cap, then stripping any trailing '-' left by truncation. Mirrors
// relabel.sh's `tr -c 'A-Za-z0-9_-' '-' | cut -c -32 | sed 's/-*$//'`.
func desiredVolumeLabel(regionShort, namespace, pvcName string) string {
	raw := regionShort + "-" + namespace + "-" + pvcName
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return fitLinodeLabel(b.String())
}

// labelTailKeep is how much of the RIGHT-hand side survives truncation. Sized to
// carry a StatefulSet ordinal plus enough of the claim name to tell siblings apart
// (`…enbao-0`, `…-db-1-wal`).
const labelTailKeep = 8

// fitLinodeLabel squeezes a label into Linode's 32-char cap while KEEPING the
// discriminating tail, by dropping from the middle rather than the end.
//
// The naive `s[:32]` this replaces cut off exactly the part that distinguishes
// sibling volumes, so an entire StatefulSet collapsed to one label:
//
//	e2e-llz-openbao-data-platform-openbao-0 ┐
//	e2e-llz-openbao-data-platform-openbao-1 ├─► "e2e-llz-openbao-data-platform-op"
//	e2e-llz-openbao-data-platform-openbao-2 ┘
//
// Linode Volume labels are account-UNIQUE, so the first replica won and the other
// two failed `PUT /v4/volumes/<id>` with 400 {"reason":"Must be unique"} — for the
// entire life of the relabeler. Observed on lke637974: 17 of 17 renames rejected,
// which is why every Volume kept its opaque pvc-<uuid> label.
//
// Truncation is still lossy, so this is not a uniqueness GUARANTEE — two claims
// agreeing on both the head and the last 8 characters would still collide. It
// removes the systematic collision (StatefulSet replicas, which differ only in the
// final character) rather than every conceivable one. The remaining risk surfaces
// as a loud per-volume error plus a red `assert-volume-encryption`, not silence.
func fitLinodeLabel(s string) string {
	if len(s) <= maxLinodeLabel {
		return strings.TrimRight(s, "-")
	}
	head := maxLinodeLabel - labelTailKeep - 1 // -1 for the joining '-'
	tail := strings.TrimLeft(s[len(s)-labelTailKeep:], "-")
	out := strings.TrimRight(s[:head], "-") + "-" + tail
	return strings.TrimRight(out, "-")
}
