# Windows support for llz: what it would take, what it would mean

Status: **draft / design** — exploratory. Nothing here is committed work; this
documents the shape of the problem and a tiered answer, not a decision.

## Summary

"Does LLZ support Windows?" is really three questions, because LLZ runs in three
places — a **CI runner**, a **remote cluster**, and an **operator workstation** —
and only the workstation is in question. CI is GitHub-Actions Linux; the cluster
is LKE-Enterprise (the operator's OS is irrelevant to it). So "Windows support"
narrows to exactly one thing: **can an operator drive the `llz` workflow from a
Windows machine?**

There are two honest answers, and they're tiers, not a yes/no:

- **It largely works *today* via WSL2 or Git Bash** — the whole flow is a Go
  binary plus standard CLIs, all of which have Linux/WSL builds, and the repo
  already ships a [Dev Container](../devcontainer.md) that *is* a Linux
  workstation. This is the cheap, real answer and it costs roughly a doc.
- **Native Windows** (run `llz.exe` in PowerShell, no Unix emulation layer) is a
  longer tail with a few sharp blockers: we publish no Windows binary, one CI
  step won't even *compile* for Windows, the pre-commit hook is a bash script,
  and `llz self-update` can't replace a running `.exe`. None are deep, but each
  is real work plus permanent two-platform maintenance.

This doc scopes the surfaces, lays out a support spectrum, inventories the
blockers by hardness (with file references), and states what each tier *costs to
keep working*, not just to build.

## What "Windows support" actually scopes to

The instinct is to think the whole platform must "support Windows." It doesn't —
most of it never touches an operator's OS:

| Surface | Where it runs | OS today | In question? |
|---|---|---|---|
| The bootstrap / day-2 CI (`llz ci …`, terraform, helm) | GitHub Actions runners | Linux | **No** — runners are Linux by selection; nothing forces them onto an operator box. |
| The cluster + apl-core | LKE-Enterprise (Linode) | Linux (remote) | **No** — the operator's OS is invisible to it. |
| The **operator workstation** flow (`llz new/env add/tokens/validate/up/status/upgrade`, the pre-commit hook) | the operator's laptop | macOS / Linux | **Yes** — this is the entire question. |
| Contributor / template-dev flow (`make build`, `make lint`, the template's own hooks) | a maintainer's laptop | macOS / Linux | Secondary — a smaller audience; treat separately. |

Two consequences fall straight out:

1. **The heavy, OS-coupled machinery is CI-only and stays Linux.** The chart
   rendering, terraform instantiation, coverage gates, and SBOM tooling in
   `template-scripts/ci/*.sh` never run on an operator workstation — the adopter
   guide's "bootstrap/operations scripts are NOT copied in" is the load-bearing
   fact here. Windows support does **not** mean porting those.
2. **The operator surface is already almost all Go.** `llz env add` replaced the
   old bash `new-deployment.sh`; the operator runs the binary and GitHub
   workflows, not shell scripts. So the native-Windows gap is small and specific,
   not a rewrite.

## The cheap answer that already works: WSL2 / Dev Container (Tier 0)

Before any code, note what an operator on Windows can do **today**:

- **WSL2** gives a real Linux userland; `install-llz.sh`, the bash pre-commit
  shim, and every CLI run unmodified. From the operator's point of view they are
  on Linux.
- **The Dev Container** ([devcontainer.md](../devcontainer.md)) is even cleaner:
  the instance repo ships `.devcontainer/devcontainer.json` pointing at a
  prebuilt multi-arch image with the entire `llz doctor` toolchain. "Open in
  container" on Windows + Docker Desktop yields the exact environment CI uses,
  zero host installs.
- **Git Bash** (ships with Git for Windows) covers the lighter case: it provides
  `bash`, `cp`, `mktemp`, `shasum`, so the install script and the hook shim work,
  though it is a thinner emulation than WSL2.

This tier is **a documentation deliverable, not an engineering one** — bless one
of these paths as the supported Windows story, test it once, and write it down.
It is almost certainly the right first (and possibly only) move.

## A spectrum of "native", not a switch

If "real" native Windows is wanted, it arrives in tiers, each independently
shippable and demand-gated by the prior one:

| Tier | Promise | Work | Keeps working by |
|---|---|---|---|
| **0 — Emulated** | "Use WSL2 or the Dev Container." | docs only | testing the documented path once |
| **1 — Native binary, core flow** | `llz.exe` runs in PowerShell: `doctor`, `env add`, `validate`, `tokens`, `status`, the read/scaffold commands. | windows build+release, the easy cross-platform gaps, self-update fix | a Windows CI build lane |
| **2 — Native gate** | The pre-commit hook works in native Git on Windows (no bash). | dual-track hook generation; `.local` hook detection | a Windows hook test |
| **3 — Native contributor** | `make`-equivalent build/lint/test on Windows. | de-bash the Makefile or provide a task runner; Windows toolchain | a full Windows CI matrix |

Most demand, if any, is satisfied at **Tier 0 or 1**. Tiers 2–3 are a long tail
that should wait for a real user asking.

## The blockers, by hardness

Grounded in the code as it stands. "Build" = stops a Windows binary from existing;
"runtime" = binary exists but a command misbehaves; "ergonomic" = works but rough.

### Build-level (must fix to ship any `llz.exe`)

- **The release matrix publishes no Windows asset.** `llz-release.yml` builds
  `{darwin,linux} × {amd64,arm64}` only (`.github/workflows/llz-release.yml:34`),
  and `install-llz.sh` maps `uname` with no Windows arm
  (`template-scripts/install-llz.sh:42-46`). Adding `windows/amd64` (+ `arm64`)
  to the matrix and an `.exe` suffix is mechanical — but it's the precondition
  for everything below.
- **One CI step won't *compile* for Windows.** `ci_harbor_steps.go:79` sets
  `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}` to detach a
  `kubectl port-forward`. `Setsid` does not exist in `syscall.SysProcAttr` on
  Windows, so `GOOS=windows go build ./...` **fails to compile the whole
  binary** — even though this step only ever runs on a CI runner and no operator
  invokes it. The fix is a build-tag split (`*_unix.go` / `*_windows.go`) behind
  a small `detachProcess()` seam. This is the cleanest illustration of the cost:
  a Linux-only *CI* concern forces a portability split in the *operator* binary,
  because Go compiles the package as a whole.

### Runtime-level (binary exists, specific commands break)

- **`llz self-update` can't replace a running `.exe`.** `selfupdate.go:272-300`
  stages a temp file and `os.Rename`s it over the live binary — safe on Unix
  (the running inode survives), impossible on Windows (the file is locked). Needs
  the rename-aside-then-replace-on-next-run pattern, or a tiny launcher. Asset
  naming (`selfupdate.go:26-29`) also assumes the Unix `llz-<os>-<arch>` scheme
  and must learn `.exe`.
- **The browser opener has no Windows arm.** `wizard.go:297-304` picks
  `open`/`xdg-open` and silently no-ops elsewhere; add `runtime.GOOS ==
  "windows"` → `rundll32 url.dll,FileProtocolHandler` (or `cmd /c start`).
  One-liner.
- **Hardcoded `/tmp`.** `ci_harbor_steps.go:32-33` pins `/tmp/harbor-pf.{log,pid}`;
  replace with `os.TempDir()` (the pattern `runner_acl.go:309-311` already uses).
  CI-path only, but trivially correct to fix.
- **Mostly already portable.** Path handling uses `filepath` throughout, password
  entry uses `golang.org/x/term` (cross-platform), `RUNNER_TEMP`/`os.TempDir()`
  fallback is already correct, and there are no `//go:build linux` files. The Go
  is in good shape — the runtime gaps are a short, enumerable list.

### Gate-level (the pre-commit hook)

- **The hook is a bash script.** `llz hooks` writes a `#!/usr/bin/env bash` shim
  (`hooks.go:43-49`) that execs `llz precommit`. Native Git on Windows runs hooks
  through its bundled bash, so this *often* works under Git for Windows — but a
  pure-PowerShell/`cmd` operator with no bash in the hook path gets nothing. A
  native story needs either a non-bash shim (a `.bat`/PowerShell variant, or
  Git's `core.hooksPath` pointing at a generated wrapper) or a documented "Git
  for Windows required" constraint. Relatedly, the `.githooks/pre-commit.local`
  escape hatch is gated on the Unix executable bit
  (`hooks.go:131`, `fi.Mode()&0o111`), which is meaningless on NTFS and would
  need a different "is this present/enabled" test.

### Ergonomic-level (contributor flow, lowest priority)

- **The Makefile is hard-bash.** `SHELL := /bin/bash` plus `xargs -0`, `cp -a`,
  and friends across dev targets. Contributors on Windows would need WSL2/Git
  Bash regardless; native `make` support means a task runner or de-bashing, and
  the audience is small. Defer hard.

## Tier 1, scoped: a native `llz.exe` with basic PowerShell support

If demand justifies going past Tier 0, this is the concrete, bounded shape of
"compile a Windows binary and give it basic PowerShell ergonomics" — chosen so it
can't sprawl into the long tail. The build side is genuinely small because the
cross-compile is already clean: the release build is `CGO_ENABLED=0` with a single
`go build -trimpath -ldflags …` in a `goos × goarch` matrix
(`.github/workflows/llz-release.yml:44-57`), so the binary is one matrix entry
away once it compiles at all.

### The one hard prerequisite

`GOOS=windows go build ./cmd/llz` **fails today** on the `Setsid` field
(`ci_harbor_steps.go:79`), so nothing ships until that is split. Extract a
`detachProcess(*exec.Cmd)` seam into two files:

- `harbor_detach_unix.go` (`//go:build !windows`) — sets
  `SysProcAttr.Setsid = true`, exactly as today.
- `harbor_detach_windows.go` (`//go:build windows`) — a no-op (or
  `CREATE_NEW_PROCESS_GROUP`); the step is CI-only and never runs on an operator
  box, so the Windows arm need only *compile*, not detach for real.

~30 lines, no behavior change on Linux. This is the gate; everything below assumes
the binary now builds.

### Build + publish

- Add `windows` to the `goos` matrix and suffix `.exe` on the Windows asset
  (`llz-windows-amd64.exe`). Two-line diff to the release workflow.
- `windows/amd64` first; `arm64` only if the amd64 lane proves out.

### The "basic PowerShell" deliverables — in scope

- **`install-llz.ps1`** — a PowerShell mirror of `install-llz.sh`: resolve the
  release, `gh release download` the `.exe` + `SHA256SUMS`, verify with
  `Get-FileHash -Algorithm SHA256`, place on PATH. ~40 lines, same shape as the
  shell version, no new concepts.
- **`llz completion powershell`** — essentially **free**: cobra's default
  `completion` command already emits a PowerShell script (the functional test
  already exercises `completion`, see `Makefile:600`). Deliverable is one doc
  paragraph: dot-source it from `$PROFILE`.
- **Runtime fixes so the `.exe` doesn't feel broken** — the browser-opener Windows
  arm (`wizard.go:297`) and `os.TempDir()` for `/tmp` (`ci_harbor_steps.go:32`),
  both one-liners noted above.
- **`llz self-update` on Windows** — the one item here that is *real* work, not a
  one-liner: Windows locks a running `.exe`, so `os.Rename` over the live binary
  (`selfupdate.go:272-300`) can't work. Implement rename-aside-then-swap (move the
  current `.exe` to `llz.exe.old`, drop the new one in place, clean up the old on
  next run) and teach `assetName` the `.exe`/`windows` scheme
  (`selfupdate.go:26-29`). Included in Tier 1 deliberately: a self-updater that
  errors on its own platform is worse than not shipping one.

### Deliberately *out* of "basic" — name the line so it holds

- **No PowerShell pre-commit hook.** Git for Windows runs hooks through its own
  bundled bash, so the existing bash shim (`hooks.go:43-49`) already works for
  anyone who installed `git` — which is everyone using this flow. Tier 1
  *documents that constraint* ("Git for Windows required for the hook") rather
  than building a `.bat`/`.ps1` hook generator. The native hook is Tier 2,
  demand-gated.
- **No PowerShell module / cmdlet wrapper** (`Invoke-Llz`, parameter binding,
  pipeline objects). `llz.exe` is already a first-class external command in
  PowerShell; a module is gold-plating.
- **No Makefile / contributor-flow port.** Separate audience, stays on WSL2.

### What Tier 1 costs to *keep*

A `windows-latest` job in CI that at minimum cross-builds and smoke-runs
`llz version` / `--help` / `completion` (the offline functional set). Without it,
the next `syscall`-shaped or `/tmp`-shaped addition silently breaks the Windows
build. Supporting the binary means testing it — that lane *is* the support.

### The caveat that survives Tier 1

A native `.exe` still leaves the operator hand-installing terraform / kubectl /
helm / gh / bao / copier on PATH — the exact friction the
[Dev Container](../devcontainer.md) erases. So even with Tier 1 shipped, the
*recommended* path stays WSL2 / Dev Container; native PowerShell is the "I genuinely
cannot run a Linux userland" fallback. The `.exe` widens the door; it does not
change which door we point people at.

## What it would mean

Beyond the one-time build, native support is a **standing commitment**, and that
is the part worth deciding deliberately:

- **Every shell-out, path, and hook becomes a two-platform surface.** Today a
  contributor can reach for `syscall.Setsid` or a bash shim without thinking;
  under a Windows promise, each such reach needs a build-tag split or a portable
  seam, forever. The `Setsid` case shows this isn't hypothetical — the constraint
  reaches into CI-only code.
- **A Windows CI lane is the only thing that keeps it true.** Without
  `GOOS=windows` in the build and at least a smoke job on a `windows-latest`
  runner, the binary regresses silently the next time someone adds a Unixism.
  Supporting Windows means *testing* Windows, which means a new CI matrix axis
  and its run-minutes.
- **The toolchain story is the operator's, not ours — but we own the docs.**
  `terraform`, `kubectl`, `helm`, `gh`, `bao`, `copier`, `jq` all have Windows
  builds, but "install these seven tools on PATH in PowerShell" is real adopter
  friction that `llz doctor` would now need to diagnose on Windows. This is
  precisely the friction the Dev Container exists to erase — which is why Tier 0
  keeps undercutting the case for going native.
- **The support contract needs a stated boundary.** "Runs on Windows" can mean
  "we don't break the build," "core operator commands are tested," or "the full
  contributor flow works." These are Tiers 1/2/3 above; picking one and saying so
  is more valuable than a vague "yes."

## Recommendation

A tiered, demand-gated path:

1. **Now (Tier 0):** officially document **WSL2 or the Dev Container** as *the*
   supported Windows operator path. Near-zero engineering, removes the real
   blocker (operator confusion), and is honest about how the flow is shaped.
2. **On real demand (Tier 1):** ship a native `llz.exe` with basic PowerShell
   ergonomics — the `Setsid` build-tag split, `windows/amd64` in the release
   matrix, `install-llz.ps1`, PowerShell completion, the self-update
   replace-on-next-run fix, and the browser/`/tmp` one-liners, behind a Windows CI
   build lane. Full breakdown of what's in and out in
   [Tier 1, scoped](#tier-1-scoped-a-native-llzexe-with-basic-powershell-support).
   This delivers a genuinely native binary for the read/scaffold/validate core
   while leaving the bootstrap heavy-lifting where it belongs (CI).
3. **Only if asked (Tiers 2–3):** native pre-commit hook generation and a
   de-bashed contributor flow. Long tail; wait for a named user.

The throughline: the platform is deliberately CI-and-cluster-centric, so the
*operator* surface that Windows actually touches is small — and a Linux
workstation-in-a-box (WSL2 / Dev Container) already covers it. Native Windows is
a finite, well-understood project, but Tier 0 likely satisfies the need at a
fraction of the standing cost.

## Open questions

- Is there a concrete operator who *cannot* use WSL2/Docker Desktop (locked-down
  Windows, no virtualization)? That single fact decides whether Tier 1 is worth
  starting.
- For a native hook, is "Git for Windows (with its bash) required" an acceptable
  constraint, or must the hook run under pure `cmd`/PowerShell?
- Does `llz doctor` grow a Windows-aware toolchain check, or do we steer all
  Windows operators into the Dev Container and check there?

## Out of scope

- Windows **CI runners** for the bootstrap/day-2 workflows — the cluster and
  apl-core are Linux; there is no reason to move CI off Linux.
- Porting `template-scripts/ci/*.sh` — CI-internal, never operator-run.
- ARM Windows beyond a matrix entry if the amd64 lane proves out.

## See also

- [devcontainer.md](../devcontainer.md) — the workstation-in-a-box that is the
  Tier-0 answer.
- [adopter-guide.md](../adopter-guide.md) — the operator flow whose surface this
  doc scopes (§2 install, §4 scaffold + hooks).
- [recipes.md](recipes.md) — the other live design doc; provider integrations
  there share the "compiled-in, per-platform driver" shape a `detachProcess()`
  seam would use.
