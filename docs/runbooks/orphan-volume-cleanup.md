# Orphan Linode Volume cleanup

The destroy pipeline's volume sweep (`llz ci reap-volumes`, run as a CI step —
it was formerly a `null_resource` destroy hook in the deleted cluster-bootstrap
Terraform root) removes Linode Block Storage Volumes tagged with this instance's
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

## ⚠️ Always pass `--env` — most orphans are not named `pvc-*`

The CSI provisions a Volume as `pvc-<uuid>`, but the in-cluster **volume-labeler**
renames it to `<env>-<namespace>-<pvc>` so it is identifiable in the Linode UI. On
any cluster that ran long enough to relabel, **most orphans no longer match the
`pvc-` prefix.**

`--env <deployment>` is what widens the sweep to include those renamed Volumes.
Without it the sweep silently matches only the CSI defaults:

<!-- llz:fact reap-volumes.env -->
```text
--env  deployment name (REGION_SHORT) whose RELABELED volumes to include; without it the sweep sees only the CSI default pvc-* labels and leaks every renamed volume
```
<!-- /llz:fact -->

```text
--env string   deployment name (REGION_SHORT) whose RELABELED volumes to include;
               without it the sweep sees only the CSI default pvc-* labels and
               leaks every renamed volume
```

That is the failure this runbook exists to fix, so omitting the flag produces a
clean-looking run that leaves behind exactly the Volumes you were paged about.
**Pass it every time**, even when you believe the cluster is too young to have
relabelled anything.

## Safe filter

A Volume is a candidate iff ALL of these are true:

| Filter | Why |
|---|---|
| `region == $REGION` | Only touch the region of the cluster you destroyed |
| `linode_id == null` | Unattached — never touch a Volume in use by ANY running Linode (including LKE clusters) |
| `label` matches `pvc-*` **or** `<env>-*` when `--env` is given | The CSI default *and* the labeler's renamed form. Without `--env`, only the first — see the warning above |
| Optional `tags` includes `$TAG_MUST_INCLUDE` | Narrow to the instance's volume-tag (or the cluster's `lke<id>` ownership tag) for a tighter blast radius |

Because the label prefix no longer separates platform Volumes from user-created
ones on its own, **the tag is the real blast-radius control.** Prefer
`--tag-must-include` over trusting the prefix whenever the account holds Volumes you
did not provision.

## Usage

Always dry-run first, eyeball every label, then re-run with confirm:

```bash
# Dry-run — lists candidates, deletes nothing
LINODE_TOKEN=<token> llz ci reap-volumes --region <cluster_region> --env <deployment>

# Once you've eyeballed the list and nothing looks like a Volume you
# still want, confirm:
LINODE_TOKEN=<token> llz --yes ci reap-volumes --region <cluster_region> --env <deployment>

# Tighter blast radius — only Volumes carrying the instance's volume-tag
# (or the destroyed cluster's lke<id> ownership tag):
LINODE_TOKEN=<token> llz --yes ci reap-volumes \
  --region <cluster_region> --env <deployment> --tag-must-include <volume-tag>
```

`LINODE_TOKEN` needs the `volumes:read_write` scope. The same
`secrets.LINODE_API_TOKEN` the Terraform destroy uses is fine.

> **Scoped, never account-wide.** At least one of `--region` / `--volume-ids` is
> required; the command refuses an unscoped sweep.

## What the dry-run looks like

```
DRY-RUN — nothing will be deleted. Re-run with --yes to delete.
=== orphan Volumes (region="<cluster_region>" volume-ids="" tag="", label prefix pvc-, unattached) ===
  would DELETE volume 12345678 (pvc-aaaaaaaaaaaaaaaa)
  would DELETE volume 12345681 (lab-monitoring-loki-data-loki-0)
  ...
  would DELETE volume 12345692 (lab-harbor-registry-data)
summary: deleted=0 failed=0
```

Eyeball every label before re-running with `--yes`; make sure none belong to a
cluster you still want. **If the list contains only `pvc-*` labels and you know the
cluster ran for a while, you probably forgot `--env`** — re-run with it and compare
the counts before deleting anything.

## What does NOT get touched

- Volumes still attached to a Linode (LKE-managed node, manual instance) — `linode_id != null`
- Volumes in any region other than `--region`
- Volumes in any other Linode account — the token is account-scoped
- Volumes whose tags exclude `--tag-must-include`, when you pass it

Note what is **not** on that list: the label prefix. It stopped being a boundary
between platform and user Volumes when relabeling shipped, so a user-created Volume
whose label happens to start with your `<env>-` is in scope. Use
`--tag-must-include` if that is a real risk in your account.

If the filter is somehow too narrow for an unusual case (e.g. cross-region
orphans), drop into the Linode UI and delete by hand.

## See also

- [`volume-labels.md`](volume-labels.md) — the labeler itself: what it renames, when, and why a Volume may still be `pvc-*` an hour after it was bound.
