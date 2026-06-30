# Recipes quickstart — write your first extension

This is a hands-on, hello-world guide to authoring an **llz extension** (a "recipe").
By the end you'll have scaffolded a recipe that drops a file into a repo, gated a commit
on a check, watched it drift and re-apply, and seen where logic-bearing recipes go.

> **"recipe" vs "extension".** Same thing. The capability is an *extension* and the
> commands are `llz extension …`, but each one's manifest file is named **`recipe.yaml`**.
> When this guide says "your recipe" it means the `recipe.yaml` + its files. For the full
> design, see [architecture/recipes.md](architecture/recipes.md).

## What a recipe is (30 seconds)

A recipe is an optional, versioned capability you can turn on in an instance. It can:

- **scaffold files** into the repo (`files:`),
- **gate your workflow** with checks (`check:` runs in `llz lint`),
- declare the **tools / vars / secrets** it needs (surfaced by `llz extension doctor`),
- and more (CI jobs, health probes, day-2 actions — see the design doc).

The whole point is that this lives *outside* the llz binary, so you add a capability
without forking the template or editing core. The golden rule: a recipe declares
**argv to already-tested tools** — never inline shell.

## Prerequisites

- `llz` on your `PATH`.
- A directory to work in (a real instance repo, or just `mkdir demo && cd demo && git init`).

## The lifecycle at a glance

```
llz extension new <name> --kind tool   # scaffold a skeleton under extensions/<name>
llz extension lint extensions/<name>   # enforce the ceiling (argv-only; kind:check ships tests)
llz extension enable <name>            # record in .llz/extensions.yaml + scaffold its files
llz lint                               # your check: steps run as part of the gate
llz extension apply --check            # report drift in scaffolded files
llz extension doctor                   # tools / vars / secrets / ghVars readiness
llz extension disable <name>           # turn off (files left in place)
llz extension teardown <name> --yes    # remove its scaffolded files
```

## 1. Scaffold your first recipe

```sh
llz extension new hello --kind tool
```

```
scaffolded extensions/hello (kind: tool)
next: edit the scaffold, then llz extension lint extensions/hello
```

That created `extensions/hello/` with a `recipe.yaml` and a `README.md`. The starter
manifest just wraps a tool:

```yaml
# extensions/hello/recipe.yaml (as scaffolded)
schemaVersion: 3
name: hello
short: TODO one-line description (wraps the hello tool)
kind: tool
stage: universal
tools: [hello]
check:
  - name: hello
    argv: [hello, --version]
```

Two fields worth knowing up front:

- **`kind`** — `tool` (a thin argv wrapper, no logic to test) or `check` (logic-bearing,
  must ship tests). Start with `tool`.
- **`stage`** — which delivery layer this targets: `iac` | `kube-infra` | `app`, or
  **`universal`** for a cross-cutting capability (the default the skeleton picks). It's
  *required*. (App-stage checks run in the app's own CI, not the platform gate — leave it
  `universal` for now.)

## 2. Make it do something — scaffold a file

Let's turn `hello` into something concrete: it drops a `HELLO.md` and checks the repo
stays friendly. Replace `recipe.yaml` with:

```yaml
schemaVersion: 3
name: hello
short: drop a friendly HELLO file and keep it friendly
kind: tool
stage: universal
files:
  - {src: files/HELLO.md, dst: HELLO.md}
check:
  - {name: greeting-present, argv: [grep, -q, hello, HELLO.md]}
```

and create the file it scaffolds:

```sh
mkdir -p extensions/hello/files
printf '# hello from <@ .name @>\n\nThis repo says hello. 👋\n' > extensions/hello/files/HELLO.md
```

The `<@ .name @>` is a **template marker**. File *bodies* are rendered (the `dst` path is
not), so a recipe can substitute values at scaffold time. Available out of the box:
`.name` (the extension name), `.instance_repo`, `.upstream_org`, plus any `vars:` you
declare. The delimiters are `<@ @>` (chosen so they never collide with GitHub Actions
`${{ }}` or bash `${ }`); the engine is Go `text/template` — simple substitution, not a
full language.

## 3. Lint and enable

The lint is the **ceiling check** — it keeps recipes honest (argv-only; logic-bearing
ones ship tests):

```sh
llz extension lint extensions/hello
# extension "hello": ok
```

Enable it. Enabling records it in `.llz/extensions.yaml` and scaffolds its files:

```sh
llz extension enable hello
```

```
enabled "hello"
scaffolded HELLO.md
extension "hello": 1 file(s) recorded in .llz/extensions.lock (...)
```

Look at the result — the body was rendered:

```sh
cat HELLO.md
# hello from hello
#
# This repo says hello. 👋
```

> You may also see `.gitattributes` appear: enabling applies the *always-on* built-in
> recipes too, so the instance is left consistent. That's expected.

## 4. Gate your workflow

Your `check:` step runs as part of the gate. Run the gate:

```sh
llz lint
# ... → grep -q hello HELLO.md
```

`grep -q hello HELLO.md` exits non-zero if `HELLO.md` loses the word "hello", so the gate
now fails a bad commit. That's the recipe contributing a real check.

**The ceiling: argv-only.** A check must call a named, tested entrypoint — you cannot
smuggle a script in. Try it:

```yaml
check:
  - {name: bad, argv: [bash, -c, "echo hi"]}
```

```sh
llz extension lint extensions/hello
# • step bad: inline shell (`bash -c …`) is rejected — call a tested entrypoint, not inline script
# llz: extension lint failed
```

(Undo that and keep the `grep` check.)

## 5. Watch it drift and re-apply

Scaffolded files are tracked. Edit the file by hand, then ask for drift:

```sh
echo "tampered" >> HELLO.md
llz extension apply --check
# extension "hello": 1 scaffold drift(s):
#   • HELLO.md (modified since scaffold)
# (exit 1)
```

Re-apply to restore the canonical content:

```sh
llz extension apply
# scaffolded HELLO.md
```

**`managed` vs `seed`.** By default a file is `managed` — the recipe owns it, re-apply
overwrites it, and `--check` reports hand-edits. If instead it's a *starter* config the
operator is meant to customize, mark it write-once so apply never clobbers it and edits
aren't drift:

```yaml
files:
  - {src: files/HELLO.md, dst: HELLO.md, mode: seed}   # written once, then operator-owned
```

## 6. Check readiness with doctor

`doctor` reports what the enabled recipes still need — missing tools, and unset
vars/secrets/ghVars:

```sh
llz extension doctor
# ⚠ hello requires "hello" — not installed; install it (its check skips until then)
# extension config: all declared vars/secrets/ghVars satisfied
```

(The warning is because our *starter* manifest still declared `tools: [hello]`; the
file-scaffolding version above dropped it. A check whose tool is absent skips with a
warning rather than failing.)

If your recipe needs inputs, declare them — `doctor` then checks them and `enable`
reminds you:

```yaml
vars:                                  # render-time inputs; feed <@ .greeting @>
  - {name: greeting, default: "hello", doc: "the word the greeting must contain"}
secrets:                               # sensitive; read from env at seed time
  - {name: HELLO_TOKEN, required: true, doc: "demo token"}
```

`vars` substitute into file bodies; `secrets` are checked for presence (and, with a
`bao:`/`ghEnv:` target, wired by `llz extension seed`). There's a third kind, `ghVars`
(non-secret GitHub Actions variables a workflow reads) — see the design doc.

## 7. Turning it off

```sh
llz extension disable hello     # stops gating; LEAVES the files in place
llz extension teardown hello --yes   # removes the scaffolded files (per the lock)
```

`disable` is deliberately non-destructive; `teardown` is the explicit, gated inverse of
scaffolding.

## When your check needs real logic

A `tool` recipe just shells to a tool. When your check needs its own logic, use
`kind: check` — and then the ceiling **requires unit tests**, so "logic is tested" travels
with the recipe:

```sh
llz extension new greet --kind check
```

```
scaffolded extensions/greet (kind: check)
next: (cd extensions/greet && go test ./...) && llz extension lint extensions/greet
```

This scaffolds a small, *testable* shape:

```
extensions/greet/
  recipe.yaml                 # check: [{argv: [go, run, ./cmd/check]}]
  internal/guard.go           # PURE logic: instance tree in → findings out
  internal/guard_test.go      # ships GREEN — lint fails a kind:check with no tests
  cmd/check/main.go           # thin I/O wrapper: exit 0 ok / 1 findings / 2 error
  .github/workflows/test.yml
```

`guard.Check(fsys)` takes the repo as an `fs.FS` and returns one string per finding
(empty = pass) — pure, so it's table-testable without a cluster. Implement your rule
there, extend `guard_test.go`, then `llz extension lint extensions/greet`.

## recipe.yaml cheat-sheet

The fields you'll reach for first (all optional except identity):

| Field | What it does |
| --- | --- |
| `schemaVersion: 3` | the current manifest schema |
| `name` / `short` | identity + one-line description |
| `kind` | `tool` (thin argv wrap) \| `check` (logic-bearing, ships tests) |
| `stage` | `iac` \| `kube-infra` \| `app` \| `universal` (required) |
| `files:` | `{src, dst, mode}` — scaffold a file or whole directory; `mode: managed`(default)\|`seed` |
| `check:` | `{name, argv}` — gate steps folded into `llz lint` (missing tool skips) |
| `vars:` | `{name, default, doc}` — render-time inputs (`<@ .name @>`) |
| `secrets:` | `{name, required, doc, bao, ghEnv}` — sensitive inputs, checked + seedable |
| `tools:` | `{name, via, version}` — external tools (`doctor` verifies; `provision` installs via mise) |

Beyond these, recipes can contribute CI jobs (`ci:`), health probes (`health:`), operator
commands (`commands:`), GitHub Actions variables (`ghVars:`), and day-2 actions (seed /
rotate / unseed / teardown). Those are covered in the design doc.

## Gotchas

- **`stage:` is required** — use `universal` for anything cross-cutting.
- **Argv-only** — no inline `sh -c "…"`; call a tested entrypoint. `kind: check` must ship
  `*_test.go`.
- **Only file *bodies* are templated**, not `dst` paths. Markers are `<@ … @>`.
- **`enable` applies the always-on built-ins too** (e.g. `.gitattributes`) so the instance
  stays consistent — that's why an unrelated file may appear.
- A recipe is **not** a Kubernetes deployment mechanism — that's a Helm chart. Recipes act
  on the *repo and its workflow*, before/independent of any cluster.

## Where to go next

- [architecture/recipes.md](architecture/recipes.md) — the full design: stages and the
  platform gate, `ci:` anchors + generated workflows, `ghVars` and the live doctor check,
  secret seeding, remote (git-pinned) recipe taps, adoption on `llz upgrade`, and the
  trust model.
- `llz extension --help` — the full command surface (`adopt`, `exclude`, `provision`,
  `sync`, `rotate`, `unseed`, `lifecycle`, `stages`, …).
