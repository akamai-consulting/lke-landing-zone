# Volume labels — why some are `pvc-<uuid>` and some are not

**The relabeler is retired. Nothing renames Linode Volumes any more, and nothing
should.** This runbook exists because clusters built before that change still
carry renamed Volumes, and those Volumes have a latent failure you need to know
about.

## Why renaming was removed

The Linode CSI driver finds a Volume's block device by **label**:
`findDevicePath` calls `key.GetNormalizedLabel()` and hands the result to
`GetDiskByIdPaths`, producing
`/dev/disk/by-id/{linode-,scsi-0Linode_Volume_}<label>`. That label comes from
the PV's `volumeHandle` (`<id>-<label>`), which is stamped at CreateVolume and is
**immutable**.

So renaming a Volume moves the in-guest udev symlink to the new name while the
driver goes on looking for the old one. It never resolves:

```
MountVolume.MountDevice failed for volume "pvc-72af5c8ff02c4813":
  Unable to find device path out of attempted paths:
  [/dev/disk/by-id/linode-pvc-72af5c8ff02c4813
   /dev/disk/by-id/scsi-0Linode_Volume_pvc-72af5c8ff02c4813]
```

The rename only breaks a Volume whose pod has **not mounted it yet**, which is
why it looked intermittent — it hit whatever happened to be rolling out when the
reconciler's first pass fired. See
[lessons-learned](../lessons-learned.md) for the full history.

## What this means for an existing cluster

A Volume that was renamed **and is currently mounted** is working, but only
because the mount already happened. Its next attach — a node drain, a pod
reschedule, a cluster upgrade — re-runs the device lookup and will not find it.

**Every renamed Volume is a deferred failure, not a cosmetic wart.**

## Finding them

`llz ci assert-volume-encryption` reports any Volume whose live label has drifted
from the label in its PV's `volumeHandle`:

```
volume 17656487 (e2e-monitoring-data-lok-gester-0) monitoring/data-loki-ingester-0 —
  label "e2e-monitoring-data-lok-gester-0" was RENAMED away from "pvc-5e9cdc9a98684924",
  the label in the PV's volumeHandle — the Linode CSI looks the device up by that
  immutable name, so this volume will fail to mount on its next attach
```

## Repairing one

There is no in-place fix. The `volumeHandle` cannot be edited, and renaming the
Volume back is only safe if you restore the label **exactly** — any drift and you
are in the same state.

Restoring the original label is the least disruptive option **when you have it**
(the gate prints it, and it is the `<label>` half of the PV's `volumeHandle`):

```bash
# The label the CSI will look for, straight from the PV:
kubectl get pv <pv> -o jsonpath='{.spec.csi.volumeHandle}'   # -> <id>-<label>
```

Then PUT that exact label back onto the Volume via the Linode API. Verify the
gate goes quiet before draining the node.

If the original label is unrecoverable, the Volume must be replaced: drain the
workload, delete the PVC, and let the StorageClass provision a new Volume.
For a replicated workload (OpenBao, a CNPG cluster, Loki ingesters) do this one
replica at a time and let it re-sync.

## What still uses the old names

Nothing writes them, but two readers still have to recognise them:

- **`llz reap`** accepts both the `pvc-` prefix and `<REGION_SHORT>-`, or renamed
  Volumes from older clusters would be invisible to the sweep and leak on
  teardown. Always pass `--env` — see
  [orphan-volume-cleanup](orphan-volume-cleanup.md).
- **`REGION_SHORT`** is still rendered into the `llz-reconciler` Deployment. No
  lane reads it now; it is retained because it is the prefix `reap` derives, and
  because dropping it would change every instance's rendered output for no
  behavioural gain.

## Attribution

Identifying which cluster owns a Volume is done with **tags**, not labels: the
`block-storage-retain` StorageClass stamps `lke<id>` at CreateVolume, and the
`volume-tags` reconciler heals any Volume born without them (clone/snapshot PVCs
admitted while admission control was degraded). Tags are not part of any device
path, so writing them is safe.
