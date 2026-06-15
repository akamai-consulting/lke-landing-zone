# Orphan Linode Volume cleanup

The cluster-bootstrap module's destroy hook
([`null_resource.cleanup_volumes_on_destroy`](../../instance-template/terraform-iac-bootstrap/cluster-bootstrap/main.tf))
sweeps Linode Block Storage Volumes tagged with this instance's
volume-tag (`<volume-tag>` — the StorageClass tag value configured for
this deployment) after PVC reap. The sweep relies on the StorageClass
actually applying that tag via its `linodebs.csi.linode.com/volumeTags`
parameter.

If the SC ever provisions Volumes WITHOUT the tag — historically because
the SC used the wrong parameter key (`/volume-tags` instead of
`/volumeTags`, fixed upstream) — the destroy-time sweep finds nothing
and the Volumes accumulate as orphans against the account quota.

**`llz ci reap-volumes`** is the manual fallback — the same volume-only sweep the
destroy workflow runs (scoped by `--region` and/or `--volume-ids`, dry-run by
default, `--yes` to delete). For an operator account-wide sweep (Volumes +
NodeBalancers + VPCs + orphan clusters, in dependency order) use **`llz reap`**
(`--region <r>`, `--cluster-label <l>`). Both share the same orphan heuristics
(`internal/linode`); no scripts checkout needed.

## When to run

- After a `terraform destroy` of a cluster provisioned by a build that
  predates the `/volumeTags` fix (Volumes are untagged, the destroy hook
  is a no-op, the Linode Volumes UI shows ~30 unattached `pvc-*` Volumes
  for the destroyed cluster's region).
- After ANY destroy that left orphans behind for any reason — e.g. the
  cluster was unreachable during destroy so the in-cluster PVC reap step
  failed before the tag sweep could even try.

## Safe filter

A Volume is a candidate iff ALL of these are true:

| Filter | Why |
|---|---|
| `region == $REGION` | Only touch the region of the cluster you destroyed |
| `linode_id == null` | Unattached — never touch a Volume in use by ANY running Linode (including LKE clusters) |
| `label` starts with `pvc-` | CSI-provisioned PVCs; excludes user-created Volumes (test disks, manual provisions) |
| Optional `tags` includes `$TAG_MUST_INCLUDE` | Once the `/volumeTags` fix is in steady state, narrow to `<volume-tag>` for a tighter blast radius |

## Usage

Always dry-run first, eyeball every label, then re-run with confirm:

```bash
# Dry-run — lists candidates, deletes nothing
LINODE_TOKEN=<token> llz ci reap-volumes --region <cluster_region>

# Once you've eyeballed the list and nothing looks like a Volume you
# still want, confirm:
LINODE_TOKEN=<token> llz --yes ci reap-volumes --region <cluster_region>

# Tighter scope once the /volumeTags fix has been live long enough that
# all new Volumes carry this instance's volume-tag:
LINODE_TOKEN=<token> llz --yes ci reap-volumes \
  --region <cluster_region> --tag-must-include <volume-tag>
```

`LINODE_TOKEN` needs the `volumes:read_write` scope. The same
`secrets.LINODE_API_TOKEN` the Terraform destroy uses is fine.

## What the dry-run looks like

```
DRY-RUN — nothing will be deleted. Re-run with --yes to delete.
=== orphan Volumes (region="<cluster_region>" volume-ids="" tag="", label prefix pvc-, unattached) ===
  would DELETE volume 15966578 (pvc-0adbc87d9a9040fe)
  would DELETE volume 15966581 (pvc-1845f14ee4904e45)
  ...
  would DELETE volume 15966592 (pvc-63ce6a5ab5e34394)
summary: deleted=0 failed=0
```

Eyeball every `pvc-*` label before re-running with `--yes`; make sure none
belong to a cluster you still want.

## What does NOT get touched

- Any user-created Volume — label doesn't start with `pvc-`
- Volumes still attached to a Linode (LKE-managed node, manual instance) — `linode_id != null`
- Volumes in any region other than `--region`
- Volumes in any other Linode account — the token is account-scoped

If the filter is somehow too narrow for an unusual case (e.g. cross-region
orphans), drop into the Linode UI and delete by hand.
