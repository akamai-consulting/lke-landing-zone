# akamai-functions (external candidate)

The reusable **Spin → Akamai Fermyon Wasm Functions** delivery kit: a CI pipeline, a
composite deploy action, a deploy script, the toolchain (spin + rust), and the Fermyon
deploy secret. It is **app-agnostic** — it does not ship any workload.

Bring your own Spin app and point `SPIN_MANIFEST` at its `spin.toml`. Workload-specific
alerts/dashboards are a separate concern (`llz extension new --kind observability`).

## Try it (from this candidate dir, before publishing)

    llz extension lint  external-candidates/akamai-functions
    llz extension apply external-candidates/akamai-functions --root /path/to/instance --dry-run

## Spin it out

Push this directory to its own repo; an instance consumes it as a pinned remote source
(`.llz/extensions.yaml` sources: + `llz extension sync`), gated on enable. `FERMYON_CLOUD_TOKEN`
seeds into OpenBao + the GH Environment; the toolchain installs via `llz extension provision`.
