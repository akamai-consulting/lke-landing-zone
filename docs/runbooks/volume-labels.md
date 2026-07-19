# Volume labels — Linode UI relabeler

The LKE-managed Linode CSI controller stamps every Block Storage Volume with
the label `pvc-<uuid>` because the `--volume-label-prefix` flag on the managed
controller is empty on LKE-Enterprise. The upstream CSI driver
(`linode-blockstorage-csi-driver` through v1.1.3) exposes no per-PVC label
mechanism — not via StorageClass parameters, not via the CSI spec's
`--extra-create-metadata`.

To get human-readable labels (e.g. `<env>-openbao-data-<release>-openbao-0`)
the in-cluster reconciler watches PVs and PUTs labels via the Linode Volumes API.

## What runs and where

This used to be a `CronJob/linode-volume-labeler` in its own namespace, seeded by
the `cluster-bootstrap` Terraform root. **Both are gone.** The CronJob was
retired into the `volume-labels` lane of the `llz-reconciler` Deployment, and the
Terraform root was deleted (ADR 0002).

| Resource | Namespace | Owner |
|---|---|---|
| `Deployment/llz-reconciler` (lane `--reconcile-volume-labels`) | `llz-reconciler` | the `llzReconciler` component, synced by Argo CD |

The lane is **watch-driven** off PVs rather than scheduled, with a resync floor
of `--volume-labels-resync` seconds (default 3600). It needs `REGION_SHORT` and
`LINODE_TOKEN`, supplied by the component's ExternalSecret.

## Label format

```
<region-short>-<namespace>-<pvc-name>
```

- `<region-short>` is `substr(var.region, 0, 3)` (a short per-cluster prefix,
  e.g. `pri`, `sec`, `sta`, or `lab`)
- Truncated to 32 characters (Linode's `LinodeVolumeLabelLength`)
- Sanitized to `[a-zA-Z0-9_-]` (the Linode label charset); any other rune
  becomes `-`
- Trailing `-` stripped after truncation

Example: `data-<release>-openbao-0` in the `openbao` namespace on a cluster
with region-short `<env>` becomes `<env>-openbao-data-<release>-openbao-0`.

## Inspecting the labeler

```bash
# Reconciler state
kubectl -n llz-reconciler get deploy llz-reconciler

# Logs for the volume-labels lane
kubectl -n llz-reconciler logs deploy/llz-reconciler | grep volume-labels
```

The lane's summary line is the quick health check:
```
summary: renamed=N already-ok=M api-404=0 errors=0
```

## Manual one-shot

Run the relabel pass directly, without waiting for a watch event or the resync
floor:

```bash
llz ci relabel-volumes      # needs REGION_SHORT + LINODE_TOKEN
```

## Why not the "first-class" path

The Linode CSI driver source ([`controllerserver_helper.go`](https://github.com/linode/linode-blockstorage-csi-driver/blob/v1.1.3/internal/driver/controllerserver_helper.go))
shows the volume label is always `<--volume-label-prefix><req.GetName()>`
where `req.GetName()` is the PV name (`pvc-<uuid>`) set by external-provisioner.
The prefix is a controller-level env var (`LINODE_VOLUME_LABEL_PREFIX`), max
12 chars, no per-PVC template.

On LKE-E the controller runs in Linode's management plane so we can't patch
its env vars from the user cluster. The two first-class paths are:

1. A Linode support ticket asking for `LINODE_VOLUME_LABEL_PREFIX` to be set
   on the managed controller (would give every volume a fixed prefix like
   `<env>-`, max 12 chars including separator; no PVC-name substitution).
2. An upstream PR adding a `linodebs.csi.linode.com/volume-label-template`
   SC parameter that uses Go-template substitution against
   `--extra-create-metadata`'s `csi.storage.k8s.io/pvc/{name,namespace}`.

This reconciler lane is the pragmatic fallback while either of those is pending.

## Disabling

Turn off the `llzReconciler` component in the spec, or drop the
`--reconcile-volume-labels` flag from the Deployment. Argo CD owns the manifest,
so an ad-hoc `kubectl` edit is reverted on the next sync — change the spec.
