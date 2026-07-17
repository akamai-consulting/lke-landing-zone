# Orphan Linode Volume cleanup

Every Volume the platform provisions carries two tags, stamped **at provision
time** by the `block-storage-retain` StorageClass's
`linodebs.csi.linode.com/volumeTags` parameter (rendered by `llz ci bootstrap-cluster`
from the cluster's numeric id):

- `block-storage` — the instance-wide sweep tag,
- `lke<cluster_id>` — the owning cluster's id, which is what makes a Volume
  *attributable*: `llz reap` / `llz ci reap-volumes` use it to decide whether a
  detached `pvc-*` Volume belongs to a live cluster (kept) or a gone one (orphan).

The destroy workflow already runs two authoritative sweeps
(`.github/workflows/llz-terraform.yml`, DESTROY Cluster job): the snapshot-scoped
`--volume-ids` sweep for Volumes attached at capture, then the
`--tag-must-include lke<id>` backstop for Volumes that were already detached.
**This runbook is the manual fallback** for when those didn't run or didn't
finish — a cancelled destroy, a hung teardown, or Volumes from a build that
predates provision-time tagging.

## How a Volume is classified

A Volume is only ever a *candidate* if it is unattached (`linode_id == null`),
labeled `pvc-*`, and inside your `--region` / `--volume-ids` scope. Candidates
then hit the cluster-liveness gate:

| Volume's `lke<id>` tag | Cluster state | Outcome |
|---|---|---|
| present | **live** | KEPT — a running cluster's Retain Volume, never an orphan |
| present | **gone** | reaped — definitive orphan |
| absent | (unknowable) | **KEPT by default (fail-safe)** — reaped only with `--reap-untagged`, and even then only when older than the 30-min grace window |

An untagged Volume has no ownership signal, so the tools refuse to guess:
without `--reap-untagged`, a plain sweep reports
`kept N untagged Volume(s) — fail-safe` and deletes nothing untagged. This is
deliberate — it protects mis-covered live Volumes at the cost of requiring an
explicit flag to clear genuinely orphaned untagged ones.

## When to run

- A destroy was cancelled/failed before its Volume sweeps completed.
- The preflight orphan census (`llz ci preflight`) warns that orphaned Volumes
  are pressing the account quota.
- Cleaning up Volumes from clusters built before provision-time tagging
  (untagged — these need `--reap-untagged`).

Note: on a LIVE cluster, an untagged PV-backed Volume usually self-heals — the
in-cluster volumeTagReconciler CronJob (`llz ci reconcile-volume-tags`, hourly)
stamps the StorageClass's volumeTags onto any Volume missing them, and reports
(never deletes) the cluster's abandoned Retain Volumes (tagged `lke<id>`,
referenced by no PV) with a ready-made `--volume-ids` reclaim command.

## Usage

Always dry-run first, eyeball every label, then re-run with `--yes`.

```bash
# Preferred: cluster-scoped by ownership tag — reaps ONLY that cluster's
# Volumes, safe even with live co-located clusters in the region:
LINODE_TOKEN=<token> llz ci reap-volumes --region <region> --tag-must-include lke<cluster_id>
LINODE_TOKEN=<token> llz --yes ci reap-volumes --region <region> --tag-must-include lke<cluster_id>

# Region sweep — tagged Volumes of GONE clusters only (live clusters' Volumes
# and ALL untagged Volumes are kept):
LINODE_TOKEN=<token> llz --yes ci reap-volumes --region <region>

# Untagged legacy Volumes (pre-tagging builds, coverage misses): opt in
# explicitly. Eyeball the dry-run hard — untagged means unattributable:
LINODE_TOKEN=<token> llz ci reap-volumes --region <region> --reap-untagged
LINODE_TOKEN=<token> llz --yes ci reap-volumes --region <region> --reap-untagged
```

For an operator account-wide sweep in dependency order (clusters → firewall →
NodeBalancers → VPCs → Volumes) use **`llz reap`** (`--region <r>`,
`--cluster-label <l>`, same `--reap-untagged` semantics). Both share the same
heuristics (`internal/linode`).

`LINODE_TOKEN` needs `volumes:read_write` (plus `lke:read_only` for the
liveness gate). The same `secrets.LINODE_API_TOKEN` Terraform uses is fine.

## What the dry-run looks like

```
DRY-RUN — nothing will be deleted. Re-run with --yes to delete.
=== orphan Volumes (region="us-ord" volume-ids="" tag="", label prefix pvc-, unattached) ===
  would DELETE volume 12345678 (pvc-aaaaaaaaaaaaaaaa)
  kept 2 detached Volume(s) tagged to a live cluster (not orphans)
  kept 3 untagged Volume(s) — no ownership tag (fail-safe; pass --reap-untagged to reap these)
summary: deleted=0 failed=0
```

Eyeball every `pvc-*` label before re-running with `--yes`; make sure none
belong to a cluster you still want.

## What does NOT get touched

- Any Volume attached to a Linode (`linode_id != null`) — in use, always safe
- Any Volume tagged `lke<id>` of a still-live cluster — a running cluster's
  Retain Volume
- Untagged Volumes, unless you pass `--reap-untagged` (and never inside their
  30-min creation grace window)
- Any user-created Volume — label doesn't start with `pvc-`
- Volumes outside `--region` / `--volume-ids`, or in another Linode account

If the filter is somehow too narrow for an unusual case (e.g. cross-region
orphans), drop into the Linode UI and delete by hand.
