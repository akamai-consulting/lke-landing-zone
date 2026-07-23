# `apl-overlay/` — apl-core-native object storage + app toggles

**These files are `llz render` output — do not hand-edit.** They are regenerated
from the spec (and the render functions in
`tools/internal/clusterspec/overlay.go`) on every `llz render`, and
`llz render --check` fails if they drift. Edit the spec (or the render code), not
these files.

This is the spec-owned, **secret-free** source of truth for the apl-core config
the landing zone drives into apl-core's *native* values:

- `obj.yaml` — apl-core's `AplObjectStorage` settings CR (`kind: AplObjectStorage`,
  config under `spec.provider.linode`: `type: linode`, region, `buckets.{loki,harbor}`,
  `spec.showWizard: false`). Lab-confirmed against apl-core v6.0.0's fixture/schema.
  The `_shared` copy carries the `${obj_access_key_id}` **placeholder** (the reconciler
  fills it from OpenBao — apl-core inlines accessKeyId from settings). There is **no
  `secretAccessKey`** field: it is an x-secret apl-core reads from the `obj-secrets`
  Secret via ESO (LLZ populates that from OpenBao). The per-env copy carries region +
  bucket names.
- `apps.yaml` — the `apps.<name>.enabled` toggles (the "AplApp" fragment). The
  `_shared` copy carries the statically-disabled apps; the per-env copy carries
  the component-driven toggles.

The in-cluster **apl-overlay reconciler** (`llz reconcile
--reconcile-apl-overlay`) reads these from the primary repo (`main`), fills the
credential placeholders from OpenBao `secret/obj/platform`, merges `_shared` +
`<env>`, and git-syncs the result onto the machine-owned `apl-<env>` branch with a
fast-forward retry — so a rotated object-storage key reaches apl-core without a
human values edit, and without a force-push. No secret is ever committed here.

See `docs/designs/apl-overlay-obj-native.md`.
