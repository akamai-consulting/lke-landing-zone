# `apl-overlay/` — apl-core-native object storage, app toggles + app values

**These files are `llz render` output — do not hand-edit.** They are regenerated
from the spec (and the render functions in
`tools/internal/shared/clusterspec/overlay.go`) on every `llz render`, and
`llz render --check` fails if they drift. Edit the spec (or the render code), not
these files.

This is the spec-owned, **secret-free** source of truth for the apl-core config
the landing zone drives into apl-core's *native* values:

- `obj.yaml` — apl-core's `AplObjectStorage` settings CR (`kind: AplObjectStorage`,
  config under `spec.provider.linode`: `type: linode`, region, `buckets.{loki,harbor}`,
  `spec.showWizard: false`). Lab-confirmed against apl-core v6.0.0's fixture/schema
  and re-confirmed unchanged at v6.1.0 (`tests/fixtures/env/settings/obj.yaml`).
  The `_shared` copy carries the `${obj_access_key_id}` **placeholder** (the reconciler
  fills it from OpenBao — apl-core inlines accessKeyId from settings). There is **no
  `secretAccessKey`** field: it is an x-secret apl-core reads from the `obj-secrets`
  Secret via ESO (LLZ populates that from OpenBao). The per-env copy carries region +
  bucket names.
- `apps.yaml` — the `apps.<name>.enabled` toggles (the "AplApp" fragment). The
  `_shared` copy carries the statically-disabled apps; the per-env copy carries
  the component-driven toggles.
- `appvalues.yaml` — the `apps.<name>._rawValues` LLZ asserts on the same AplApp
  CRs. **`_shared` only — there is no per-env copy**, because every entry is a
  platform decision no environment should differ on. It is the *only* channel
  that can set an apl-core app's chart values on the managed platform: the base
  `apl-values/values.yaml` that used to carry them is not rendered on managed
  (`llz render` stopped emitting one at the App Platform pivot, ADR
  [0005](https://github.com/akamai-consulting/lke-landing-zone/blob/main/docs/adr/0005-managed-app-platform.md)),
  and settings left there reach no cluster. Today it carries Argo CD's
  `resource.customizations.health` overrides (without which a fresh bootstrap can
  wedge before OpenBao) and Loki's ingester WAL-replay resources + WAL PVC.

  **A key that names nothing in the chart is silently ignored** — that is the
  failure this file was created in response to, and living on a real channel does
  not make it self-verifying. Every entry must be backed by a gate that reads the
  *consumer* (Loki's is `llz ci assert-loki`, which holds the running ingester to
  the asserted limit and fails naming what it actually found). Do not add one
  without naming its gate.

The in-cluster **apl-overlay reconciler** (`llz reconcile
--reconcile-apl-overlay`) reads these from the primary repo (`main`), fills the
credential placeholders from OpenBao `secret/obj/platform`, merges `_shared` +
`<env>`, and git-syncs the result onto the machine-owned `apl-<env>` branch with a
fast-forward retry — so a rotated object-storage key reaches apl-core without a
human values edit, and without a force-push. No secret is ever committed here.

See `docs/designs/apl-overlay-obj-native.md`.
