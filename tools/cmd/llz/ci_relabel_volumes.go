package main

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
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

func ciRelabelVolumesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "relabel-volumes",
		Short: "rename Linode Volumes to <region>-<ns>-<pvc> for every bound Linode-CSI PV",
		Long: "In-cluster Linode Volume relabeler — the Go port of the linode-volume-labeler\n" +
			"relabel.sh CronJob. Lists cluster PVs and, for each bound Linode-CSI volume,\n" +
			"rewrites its Linode UI label from the CSI default pvc-<uuid> to a readable\n" +
			"<REGION_SHORT>-<namespace>-<pvc-name> (sanitized to Linode's charset, truncated\n" +
			"to 32 chars). Idempotent — already-correct labels are skipped; a volume deleted\n" +
			"out-of-band (absent from the account list) is skipped. Env: REGION_SHORT,\n" +
			"LINODE_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runRelabelVolumes(context.Background()) },
	}
}

func runRelabelVolumes(ctx context.Context) error {
	regionShort := os.Getenv("REGION_SHORT")
	if regionShort == "" {
		return fmt.Errorf("REGION_SHORT must be set (e.g. pri|sec|sta|lab)")
	}
	token := inclusterLinodeToken()
	if token == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (env or the optional linode-api-token Secret volume)")
	}

	k, err := discoverKubeFn()
	if err != nil {
		return err
	}
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

// linodeCSIVolumes extracts a csiVolume for every PV backed by the Linode CSI
// driver that has a bound claimRef (orphaned PVs — no claimRef — are skipped, as
// are non-Linode-CSI PVs). The Volume id is the leading segment of the CSI
// volumeHandle (`<id>-<name>` on LKE-E).
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
	s := b.String()
	if len(s) > maxLinodeLabel {
		s = s[:maxLinodeLabel]
	}
	return strings.TrimRight(s, "-")
}
