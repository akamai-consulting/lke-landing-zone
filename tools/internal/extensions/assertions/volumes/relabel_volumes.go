package volumes

// ci_relabel_volumes.go implements `llz ci relabel-volumes`. It USED to rewrite
// every bound Linode-CSI Volume's label from the CSI default (`pvc-<uuid>` on
// LKE-E, where the managed CSI controller's --volume-label-prefix is empty) to a
// readable `<REGION_SHORT>-<namespace>-<pvc-name>`. It no longer renames
// anything, and the rename must not come back.
//
// RENAMING A BOUND CSI VOLUME PERMANENTLY BREAKS ITS NEXT MOUNT. The Linode CSI
// driver finds the block device by LABEL, not by id: findDevicePath does
// `deviceName := key.GetNormalizedLabel()` and hands it to GetDiskByIdPaths,
// producing /dev/disk/by-id/{linode-,scsi-0Linode_Volume_}<label>. That label
// comes from the volumeHandle (`<linode-id>-<label>`), which is stamped at
// CreateVolume and is IMMUTABLE. Rename the Volume and the in-guest udev symlink
// follows the new label while the driver goes on hunting the old one — forever.
// The kubelet reports it as, and it never resolves:
//
//	MountVolume.MountDevice failed for volume "pvc-72af5c8ff02c4813":
//	  Unable to find device path out of attempted paths:
//	  [/dev/disk/by-id/linode-pvc-72af5c8ff02c4813
//	   /dev/disk/by-id/scsi-0Linode_Volume_pvc-72af5c8ff02c4813]
//
// HOW IT WENT UNNOTICED FOR WEEKS. The damage needs the rename to land between
// AttachVolume and MountDevice, so it only hits volumes whose pod has not mounted
// YET. Until e2eb26fb the lane had no token-arrival kick, so on a cold bootstrap
// it first ran at the 1h resync floor — by which time everything had long since
// mounted. That hour was never meant as a safety margin, but it was one: the kick
// moved the first pass into the middle of bootstrap, straight through the mount
// window of every later-wave chart. On e2e that is loki-ingester, reproduced 3/3
// (runs 33215535380, 33222076304, 33225578330); in the field it showed as "1 of 3
// ingesters ready for 16 days", which the only alert watching it could not fire on
// (wrong StatefulSet name AND `== 0` vs a partial outage — see
// support-plane-alerts.yaml). docs/lessons-learned.md recorded the symptom as an
// intermittent Linode CSI flake; it is not, it is this lane.
//
// THERE IS NO SAFE SUBSET. Skipping volumes that are not yet mounted would still
// leave every renamed volume unmountable on its NEXT attach — a node drain, a
// reschedule, an upgrade — because the handle keeps the creation-time label
// either way. A rename is only ever safe on a Volume no CSI PV will mount again,
// which is a Volume about to be reaped, where a readable name buys nothing.
//
// NOTHING FUNCTIONAL DEPENDED ON THE RENAME. Cluster identity lives in Volume
// TAGS (`lke<id>`, stamped by the block-storage-retain StorageClass's volumeTags
// at CreateVolume, with reconcile-volume-tags as the heal-path backstop), and the
// reaper accepts the `pvc-` prefix, so volumes keeping their CSI default stay
// reapable. The readers still tolerate the OLD renamed form
// (linode.VolumeLabelPrefixes) because clusters relabelled before this change
// still carry those names — that tolerance is for reading history, not a licence
// to write it again.
//
// What remains is a REPORT: it says what the old lane would have renamed, so an
// operator can see the lane is deliberately inert rather than quietly missing.
import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
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
var relabelLinodeFn = func(token string) volumeLabeler {
	return capability.CloudFor(cloudBinding("volume-labels")).Client(token, 30*time.Second)
}

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

	// NO UpdateVolumeLabel CALL, deliberately — see the file header. Every volume
	// this loop would once have renamed is one the CSI will look up by the label it
	// has now, so renaming it is what breaks the mount.
	var wouldRename, historic, missing int
	for _, v := range vols {
		desired := desiredVolumeLabel(regionShort, v.namespace, v.pvcName)
		cur, present := labelByID[v.id]
		switch {
		case !present:
			missing++ // volume deleted out-of-band while the PV still references it
		case cur == desired:
			historic++ // renamed by a build that predates this change
		default:
			wouldRename++
		}
	}
	fmt.Printf("summary: renaming-disabled=%d already-renamed=%d missing=%d\n", wouldRename, historic, missing)
	fmt.Println("relabel-volumes is inert BY DESIGN: the Linode CSI resolves a volume's device " +
		"path from its label, and the label in the PV's volumeHandle is immutable, so renaming a " +
		"bound Volume permanently breaks its next mount. Cluster identity comes from Volume tags.")
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
// entire life of the relabeler — observed live, 17 of 17 renames rejected, which
// is why every Volume kept its opaque pvc-<uuid> label.
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
