# External extension candidates

Staging ground for **remote** llz extensions before they are spun out into their own
git repositories. Each subdirectory is a self-contained extension (a `recipe.yaml` plus a
`files/` scaffold tree). Nothing here is compiled into the binary or enabled by default —
these are NOT built-ins.

## Lifecycle of a candidate

1. Develop + `llz extension lint <dir>` here.
2. Dry-run the scaffold against an instance: `llz extension apply <dir> --root <instance>`.
3. Spin it out: push the subdir to its own repo, then an instance consumes it as a pinned
   remote source (`.llz/extensions.yaml` sources: + `llz extension sync`), gated on enable.

## Candidates

- **akamai-functions** — Rust/Spin OHTTP gateway+relay deployed to Akamai (Fermyon Wasm
  Functions). The forcing-function workload kit: scaffolds a multi-crate app tree, its CI
  pipeline, the deploy action/script, and observability content; declares its deploy
  secrets + toolchain.
