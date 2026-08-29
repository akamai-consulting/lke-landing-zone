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

## Repairing one — without losing the data

The rule is simply that **the label half of the PV's `volumeHandle` must equal
the Volume's actual label.** A rename breaks that equality; a repair restores it.
There are two ways round, and neither destroys data.

First, read what the driver is looking for:

```bash
kubectl get pv <pv> -o jsonpath='{.spec.csi.volumeHandle}'   # -> <id>-<label>
```

**Option A — put the label back.** Least disruptive when you still have the
original (the gate prints it, and it is the `<label>` half above). `PUT
/v4/volumes/<id>` with that **exact** string; any drift and you are in the same
state. Verify the gate goes quiet before draining the node.

**Option B — rebuild the PV so the handle matches the label.** Use this when the
original label is unrecoverable, or when you would rather keep the readable name.
The handle is free-form text after the first dash — `ParseLinodeVolumeKey` does
`strings.SplitN(key, "-", 2)` — and nothing validates it against the Linode API at
mount time. So the handle can be re-stated to match reality.

The `volumeHandle` field is immutable on a live PV, so this means recreating the
PV **object**. With `persistentVolumeReclaimPolicy: Retain` the Linode Volume
survives that, and the PVC rebinds:

1. Confirm the PV is `Retain`. **If it is `Delete`, patch it to `Retain` first** —
   deleting the PV object under `Delete` destroys the Volume.
2. Record the full PV spec, the Volume id, and the Volume's current label.
3. Delete the PV object. The Volume persists; the PVC goes `Pending`.
4. Recreate the PV with `csi.volumeHandle: <id>-<current-label>` and the same
   `claimRef` (namespace, name, and the PVC's uid) so it rebinds to the same PVC.
5. Restart the workload and confirm it mounts.

Do this **one replica at a time** on a replicated workload (OpenBao, a CNPG
cluster, Loki ingesters), and let each re-sync before the next.

> **Not a reconciler job.** Both options are deliberate, per-Volume, operator-run
> repairs. Automating them means mutating PV objects in lockstep with Linode API
> calls and being correct under every partial failure — which is how you get
> half-repaired Volumes with no path back. That is the same reasoning that
> retired the relabeler.

**Replacing the Volume is the last resort, not the first.** Draining the workload
and deleting the PVC discards the data; the options above do not. Reach for it
only when the Volume is genuinely unwanted.

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

## Attribution — it lives in tags now

Identifying a Volume is done with **tags**, never labels. Tags are not part of any
device path, so writing one cannot break a mount the way renaming a Volume does.

| Tag | Set by | Answers |
|---|---|---|
| `block-storage`, `platform-support-services` | `block-storage-retain` StorageClass, at CreateVolume | is this ours? |
| `lke<id>` | same | which cluster? |
| `ns-<namespace>` | `volume-tags` reconciler | which namespace? (filterable in Cloud Manager) |
| `<namespace>-<pvc>` | same | **which workload?** |

The last two carry what the old label carried. They are written after creation by
the `volume-tags` lane, which is safe precisely because tags are inert — the lane
echoes the Volume's existing label back on every PUT and changes only the tag set.

Two differences from the old label, both improvements. Tags need not be
account-unique, so StatefulSet replicas no longer collide (the old scheme had
three OpenBao replicas competing for one name, and Linode rejected 17 of 17
renames with `Must be unique`). And a wrong tag is correctable, where a wrong
label was permanent.

They do not carry `REGION_SHORT`. `lke<id>` identifies the cluster more precisely
than a three-letter prefix, and the `volume-tags` lane deliberately takes no
per-env configuration.

### What survives a deleted cluster

Tags live on the Linode Volume, not on the cluster, so they outlive it.

`lke<id>` is stamped by the StorageClass **at CreateVolume**, before the Volume is
ever observable — there is no reconcile-lag window, which is exactly why `llz
reap`'s cluster-liveness gate can key on it. So a Volume leaked from a cluster
that no longer exists still names the cluster that made it.

The per-workload tags (`ns-*`, `<namespace>-<pvc>`) are written by the
`volume-tags` lane *after* creation, so those do have a lag window: a Volume
created and orphaned before the lane's next pass carries `lke<id>` but no workload
name. In practice the lane runs on every PV event, so the window is short.

The one path that can produce a Volume with **no** tags at all is a clone/snapshot
PVC admitted while admission control is degraded — the Linode CloneVolume API
takes no tags and does not copy the source's. That is precisely what the
`volume-tags` lane exists to heal, and a Volume orphaned inside that window is the
genuinely unattributable case.

Readable **labels** would need the CSI driver to build them at CreateVolume from
the PVC's identity, which is the only point where the label and the volumeHandle
cannot diverge. Requested upstream in
[linode-blockstorage-csi-driver#603](https://github.com/linode/linode-blockstorage-csi-driver/issues/603),
with the platform half in
[apl-core#3607](https://github.com/linode/apl-core/issues/3607).
