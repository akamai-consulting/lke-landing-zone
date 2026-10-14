# `llz-wedge-gameday` — maintainer rationale

`instance-template/.github/workflows/llz-wedge-gameday.yml` is the reusable
(`workflow_call`) blast-radius game-day. It is **vendored verbatim into every
customer instance by copier**, alongside the composite actions it calls, so the
job runs self-contained with no cross-repo checkout. It follows the same
surface-reduction, single-checkout and cluster-access pattern as
`llz-cluster-health.yml` (which see, and
`docs/adr/0003-vendor-actions-and-bodies-into-instances.md`): the instance ships
a ~20-line caller stub and vendors this body plus the composite actions.

Because the YAML is copied into instances where it can never be updated in
place, long-form maintainer archaeology lives here in the template repo instead
of in the workflow body. This document is the archive; the inline comments are
the 3am debugging aids.

---

## What it proves

It breaks ONE platform `ExternalSecret` on a live (warm) cluster and asserts the
wedge is **contained** to the carved Application that owns it —
`platform-bootstrap` and every sibling carved App stay Healthy
(`docs/designs/blast-radius-decomposition.md`). This is the concrete
before/after proof of the **#163** containment claim.

It is safe to run repeatedly: `llz ci wedge-gameday` always restores the
`ExternalSecret` (and Argo self-heal reverts it too), so the cluster is left as
found.

## When to run it

Against a warm cluster that the converge-only fast-path (**#146**) keeps up, or
after an e2e provision with `keep_cluster=true`.

---

## Inputs and secrets

### Defaults

`target-app` defaults to `llz-observability` and `externalsecret` to
`apl-secrets/obj-secrets` — the carved App and a secret it owns.

The default used to be `monitoring/loki-object-store`, which is the pairing the
containment claim was first demonstrated with and stopped existing when object
storage went apl-core-native: that ExternalSecret was deleted, so a default-args
run broke nothing and proved nothing. A fault-injection drill whose fault cannot
be injected is worse than no drill, because it reports a clean containment result.
`obj-secrets` carries the object-storage secret half apl-core reads, so breaking
it produces the real wedge.

---

## Job: `gameday`

### Step: Checkout the instance (TF roots)

Single checkout at the workspace root, for the same reason as
`llz-cluster-health.yml`: the `fetch-kubeconfig` composite action hard-codes
`working-directory: terraform-iac-bootstrap/cluster` and a composite action's
working directory resolves against the workspace root.

### Step: Wedge game-day (fault-injection containment proof)

Dispatch inputs are passed via `env:` and never interpolated as `${{ }}` into
the script body — script-injection hygiene.

The 180s wait loop lets the control-plane ACL propagate before the first
apiserver call. It absorbs exactly the transient described at length in
`docs/workflows/llz-cluster-health.md`: the cluster-access step opened this
runner's egress IP moments earlier, and a fresh ACL change takes tens of seconds
to take effect, so an immediate probe fails on a cluster that is actually up.

The step exits with the game-day's own exit code, after writing the before/after
proof into `$GITHUB_STEP_SUMMARY`.

### Step: Revoke control-plane ACL for this runner

`if: always()` — closes the egress-IP hole opened by cluster-access, whatever
the outcome of the game-day.
