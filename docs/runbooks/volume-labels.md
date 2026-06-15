# Volume labels — Linode UI relabeler

The LKE-managed Linode CSI controller stamps every Block Storage Volume with
the label `pvc-<uuid>` because the `--volume-label-prefix` flag on the managed
controller is empty on LKE-Enterprise. The upstream CSI driver
(`linode-blockstorage-csi-driver` through v1.1.3) exposes no per-PVC label
mechanism — not via StorageClass parameters, not via the CSI spec's
`--extra-create-metadata`.

To get human-readable labels (e.g. `<env>-openbao-data-<release>-openbao-0`)
we run a CronJob that walks the cluster's PVs and PUTs labels via the Linode
Volumes API.

## What runs and where

| Resource | Namespace | Provisioner |
|---|---|---|
| `CronJob/linode-volume-labeler` | `linode-volume-labeler` | `instance-template/terraform-iac-bootstrap/cluster-bootstrap` |
| `ConfigMap/linode-volume-labeler-script` | `linode-volume-labeler` | TF (script content from `manifests/linode-volume-labeler/relabel.sh`) |
| `Secret/linode-api-token` | `linode-volume-labeler` | TF, sourced from `var.linode_token` |
| `NetworkPolicy/linode-volume-labeler-egress` | `linode-volume-labeler` | TF — restricts egress to DNS + `api.linode.com` (TCP/443) + apiserver (TCP/6443) |

The CronJob schedule is `*/15 * * * *` with `concurrencyPolicy: Forbid` and
`startingDeadlineSeconds: 600`.

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
# CronJob state
kubectl -n linode-volume-labeler get cronjob

# Last few Jobs (failedJobsHistoryLimit=3, successfulJobsHistoryLimit=1)
kubectl -n linode-volume-labeler get jobs

# Pod logs for the most recent Job
kubectl -n linode-volume-labeler logs --tail=200 -l app.kubernetes.io/component=relabel
# or
kubectl -n linode-volume-labeler logs job/$(kubectl -n linode-volume-labeler get jobs -o name | tail -1 | cut -d/ -f2)
```

The Job log summary line is the quick health check:
```
summary: renamed=N already-ok=M api-404=0 errors=0
```

`errors > 0` produces a non-zero pod exit code so the Job is marked Failed.

## Manual one-shot

Create a Job directly from the CronJob template to relabel immediately
without waiting for the next 15-minute slot:

```bash
kubectl -n linode-volume-labeler create job \
  --from=cronjob/linode-volume-labeler \
  manual-relabel-$(date +%s)
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

This CronJob is the pragmatic fallback while either of those is pending.

## Disabling

Suspend the CronJob:
```bash
kubectl -n linode-volume-labeler patch cronjob linode-volume-labeler \
  -p '{"spec":{"suspend":true}}'
```

Re-enable by patching `"suspend":false`. The TF resource will reconcile the
spec back to `false` on the next apply unless the CronJob YAML is also edited.

To remove entirely, delete the TF resources in
[`instance-template/terraform-iac-bootstrap/cluster-bootstrap/main.tf`](../../instance-template/terraform-iac-bootstrap/cluster-bootstrap/main.tf)
under the `# Linode Volume relabeler` block and `terraform apply`.
