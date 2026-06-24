#!/usr/bin/env bash
# llz-functional.sh — functional test of the built `llz` binary: drive it the way
# an adopter does and assert on real behaviour, not mocked argv. Complements the
# in-process unit tests (tools/cmd/llz/*_test.go, which stub the shell-out) and
# scaffold-render-check.sh (which already covers `llz env add`).
#
# Two sections:
#
#   A. Basic commands (OFFLINE, always run) — the built binary boots and its
#      core verbs behave: version/--version/--help, `self-update --help`, shell
#      completion, `env list --json` on an empty tree, and that a bad env name
#      and an unknown subcommand both exit non-zero. No network, no gh, no cloud.
#
#   B. Install + self-update flow (NETWORK + gh) — the path docs/quickstart.md §2
#      documents and that a 404 complaint surfaced: the template repo is PRIVATE,
#      so the install must be authenticated. Asserts, against a real published
#      release:
#        1. the documented `gh release download` + checksum verify succeeds;
#        2. `llz self-update` on a copy of the binary downloads → checksum-verifies
#           → atomically replaces it, and the replaced binary runs and reports the
#           release version;
#        3. a second `self-update` is idempotent ("already on …").
#      This section needs `gh` authenticated to the template repo. It SELF-SKIPS
#      when gh is unauthenticated (so `make llz-functional` still runs section A
#      offline); CI authenticates via GITHUB_TOKEN. Set LLZ_FUNCTIONAL_NET=1 to
#      require it (skip => failure), or =0 to force-skip.
#
# Usage: template-scripts/ci/llz-functional.sh
# Env:
#   LLZ                  path to the llz binary       (default: bin/llz, llz on PATH, else build)
#   LLZ_FUNCTIONAL_REPO  template repo for section B   (default: akamai-consulting/lke-landing-zone)
#   LLZ_FUNCTIONAL_REF   release to install, vX.Y.Z    (default: latest vX.Y.Z on the repo)
#   LLZ_FUNCTIONAL_NET   1=require section B, 0=skip   (default: auto — run iff gh is authed)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REPO="${LLZ_FUNCTIONAL_REPO:-akamai-consulting/lke-landing-zone}"

# Prefer a prebuilt bin/llz, an llz on PATH, else build one (same resolution as
# scaffold-render-check.sh).
if [[ -n "${LLZ:-}" ]]; then :
elif [[ -x "$ROOT/bin/llz" ]]; then LLZ="$ROOT/bin/llz"
elif command -v llz >/dev/null 2>&1; then LLZ="$(command -v llz)"
else
  echo "Building llz (no bin/llz or llz on PATH)…" >&2
  ( cd "$ROOT/tools" && go build -o "$ROOT/bin/llz" ./cmd/llz )
  LLZ="$ROOT/bin/llz"
fi
LLZ="$(cd "$(dirname "$LLZ")" && pwd)/$(basename "$LLZ")"   # absolutise (we cd into temp dirs)

step() { printf '\n\033[1m── %s\033[0m\n' "$1"; }
pass() { echo "  ok   $1"; }
fail() { echo "::error::$1"; FAILED=1; }
FAILED=0

# Run the basic commands from an empty dir: llz reads .llz/commands.yaml and the
# instance layout from cwd, so an empty cwd keeps section A hermetic (and proves
# `env list` returns [] rather than erroring when nothing is scaffolded).
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

# ── Section A: basic commands (offline) ───────────────────────────────────────
step "A. Basic commands (offline)"

# version / --version: must print a non-empty "llz <version>" line.
out="$("$LLZ" version)"
if [[ "$out" == llz\ * ]]; then pass "version → '$out'"; else fail "version: unexpected output '$out'"; fi
out="$("$LLZ" --version)"
if [[ -n "$out" ]]; then pass "--version → '$out'"; else fail "--version printed nothing"; fi

# --help: the core verbs must be advertised.
help="$("$LLZ" --help 2>&1)"
for verb in env doctor tokens build status self-update version; do
  if grep -qE "^[[:space:]]+${verb}\b" <<<"$help"; then
    pass "--help lists '$verb'"
  else
    fail "--help does not list subcommand '$verb'"
  fi
done

# self-update --help: documents the gh-authenticated, checksum-verified path.
suhelp="$("$LLZ" self-update --help 2>&1)"
if grep -qiE 'checksum|sha256' <<<"$suhelp" && grep -qi 'gh' <<<"$suhelp"; then
  pass "self-update --help mentions gh + checksum/SHA256"
else
  fail "self-update --help should describe the gh/checksum-verified path"
fi

# completion: cobra emits a real shell-completion script.
if "$LLZ" completion bash 2>/dev/null | grep -qi 'bash completion'; then
  pass "completion bash emits a completion script"
else
  fail "completion bash did not emit a completion script"
fi

# env list on an empty tree: no deployments scaffolded → exactly '[]' (the shape
# the CI matrices consume), exit 0.
out="$("$LLZ" env list --json)"
if [[ "$out" == "[]" ]]; then pass "env list --json → []"; else fail "env list --json on empty tree = '$out', want []"; fi

# Invalid env name must be rejected before any work (exit non-zero, no writes).
if "$LLZ" env add 1bad --region us-ord --obj-cluster us-ord-1 >/dev/null 2>&1; then
  fail "env add accepted invalid name '1bad' (expected non-zero exit)"
else
  pass "env add rejects invalid name '1bad'"
fi

# Unknown subcommand must fail (cobra: 'unknown command').
if "$LLZ" definitely-not-a-real-command >/dev/null 2>&1; then
  fail "unknown subcommand exited 0 (expected non-zero)"
else
  pass "unknown subcommand exits non-zero"
fi

# ── Section B: install + self-update flow (network + gh) ──────────────────────
step "B. Install + self-update flow (repo=$REPO)"

# Decide whether to run: explicit LLZ_FUNCTIONAL_NET wins; else auto-detect gh auth.
run_net="${LLZ_FUNCTIONAL_NET:-auto}"
# Scope to one host: bare `gh auth status` fails if any configured host is broken.
gh_ok() { command -v gh >/dev/null 2>&1 && gh auth status --hostname "${GH_HOST:-github.com}" >/dev/null 2>&1; }
case "$run_net" in
  0) echo "  skipped (LLZ_FUNCTIONAL_NET=0)"; SKIP_NET=1 ;;
  1) gh_ok || { fail "LLZ_FUNCTIONAL_NET=1 but gh is not authenticated"; }; SKIP_NET=0 ;;
  *) if gh_ok; then SKIP_NET=0; else echo "  skipped (gh not authenticated; set LLZ_FUNCTIONAL_NET=1 to require)"; SKIP_NET=1; fi ;;
esac

if [[ "${SKIP_NET:-1}" -eq 0 ]]; then
  # Resolve the release under test: an explicit ref (v0.1.0 / 0.1.0, or a legacy
  # llz/v0.1.0), else the highest-semver bare vX.Y.Z umbrella tag on the repo.
  REF="${LLZ_FUNCTIONAL_REF:-}"
  if [[ -n "$REF" ]]; then
    v="${REF#llz/}"; v="${v#v}"; TAG="v${v}"
  else
    # Highest-semver FULL release: skip drafts and pre-releases (unpromoted e2e
    # candidates), matching latestRelease() in selfupdate.go.
    TAG="$(gh release list --repo "$REPO" --limit 200 --json tagName,isDraft,isPrerelease --jq \
      '[.[] | select((.isDraft|not) and (.isPrerelease|not)) | .tagName | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))] | sort_by(ltrimstr("v") | split(".") | map(tonumber)) | last')"
    [[ -n "$TAG" && "$TAG" != "null" ]] || { fail "no full vX.Y.Z release found on $REPO"; TAG=""; }
  fi
  WANT_VER="$TAG"   # e.g. v0.1.0

  # Asset for THIS platform (matches assetName() in selfupdate.go).
  goos="$(go env GOOS 2>/dev/null || uname -s | tr '[:upper:]' '[:lower:]')"
  goarch="$(go env GOARCH 2>/dev/null || true)"
  if [[ -z "$goarch" ]]; then
    case "$(uname -m)" in
      x86_64) goarch=amd64 ;;
      arm64|aarch64) goarch=arm64 ;;
      *) goarch="$(uname -m)" ;;
    esac
  fi
  ASSET="llz-${goos}-${goarch}"

  if [[ -n "$TAG" ]]; then
    echo "  release=$TAG asset=$ASSET"
    # sha256 verifier: macOS ships `shasum`, Linux ships `sha256sum`.
    if command -v shasum >/dev/null 2>&1; then SHA_C=(shasum -a 256 -c -); else SHA_C=(sha256sum -c -); fi
    sha_check() { # verify $ASSET (in cwd) against the SHA256SUMS in the same dir
      grep " ${ASSET}\$" SHA256SUMS | "${SHA_C[@]}" >/dev/null 2>&1
    }

    # B1. Documented install #1 — `gh release download` + checksum verify, then
    # the downloaded binary actually runs and reports the release version.
    step "B1. install via gh release download + checksum (quickstart §2, primary)"
    dl="$(mktemp -d)"
    if ( cd "$dl" && gh release download "$TAG" --repo "$REPO" --pattern "$ASSET" --pattern SHA256SUMS --clobber ) \
       && ( cd "$dl" && sha_check "$ASSET" ); then
      chmod +x "$dl/$ASSET"
      got="$("$dl/$ASSET" version 2>&1 || true)"
      if [[ "$got" == *"$WANT_VER"* ]]; then
        pass "gh-downloaded $ASSET verified + runs ('$got')"
      else
        fail "gh-downloaded binary did not report $WANT_VER (got '$got')"
      fi
    else
      fail "gh release download $TAG ($ASSET) failed or checksum mismatch — is gh authed for $REPO?"
    fi
    rm -rf "$dl"

    # B2. Documented install #2 — authenticated `curl` against the PRIVATE repo.
    # This is the exact flow from the 404 complaint: the browser download URL 404s
    # on a private repo regardless of token, so the install must hit the API asset
    # endpoint with Authorization + Accept: application/octet-stream. Assert (a) the
    # anonymous browser URL really does 404 (the reason auth is required) and (b)
    # the authenticated API-endpoint curl yields a runnable binary.
    step "B2. install via authenticated curl on the private repo (quickstart §2, curl)"
    BROWSER_URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
    code="$(curl -fsS -o /dev/null -w '%{http_code}' "$BROWSER_URL" 2>/dev/null || true)"
    if [[ "$code" == "404" ]]; then
      pass "anonymous browser URL 404s on the private repo (auth required) — code=$code"
    else
      echo "  note: anonymous browser URL returned code=$code (repo may now be public)"
    fi
    cdir="$(mktemp -d)"
    ASSET_ID="$(gh api "repos/${REPO}/releases/tags/${TAG}" --jq ".assets[] | select(.name==\"${ASSET}\") | .id" 2>/dev/null || true)"
    if [[ -n "$ASSET_ID" ]] && curl -fsSL \
         -H "Authorization: Bearer $(gh auth token)" \
         -H "Accept: application/octet-stream" \
         "https://api.github.com/repos/${REPO}/releases/assets/${ASSET_ID}" -o "$cdir/llz" 2>/dev/null; then
      chmod +x "$cdir/llz"
      got="$("$cdir/llz" version 2>&1 || true)"
      if [[ "$got" == *"$WANT_VER"* ]]; then
        pass "authenticated curl (API asset endpoint) yields a runnable $WANT_VER binary ('$got')"
      else
        fail "curl-installed binary did not report $WANT_VER (got '$got')"
      fi
    else
      fail "authenticated curl install failed (asset id resolve or download) for $ASSET@$TAG on $REPO"
    fi
    rm -rf "$cdir"

    # B3. self-update: download → checksum → atomic replace, on a copy of the
    # binary; the replaced binary must run and report the release version.
    step "B3. llz self-update replaces the binary and it runs"
    sbx="$(mktemp -d)"
    cp "$LLZ" "$sbx/llz"; chmod +x "$sbx/llz"
    if "$sbx/llz" self-update --repo "$REPO" --ref "$WANT_VER" >/dev/null 2>&1; then
      got="$("$sbx/llz" version)"
      if [[ "$got" == *"$WANT_VER"* ]]; then
        pass "self-update installed $WANT_VER; replaced binary reports '$got'"
      else
        fail "after self-update, version = '$got', want it to contain '$WANT_VER'"
      fi
      # B4. Idempotent: now on the target, a second run is a no-op.
      if "$sbx/llz" self-update --repo "$REPO" --ref "$WANT_VER" 2>&1 | grep -qi 'already on'; then
        pass "self-update is idempotent (already on $WANT_VER)"
      else
        fail "second self-update should report 'already on $WANT_VER'"
      fi
    else
      fail "self-update --ref $WANT_VER failed against $REPO"
    fi
    rm -rf "$sbx"

    # B5. Dry-run resolves a target without installing.
    step "B5. self-update --dry-run resolves a target"
    if "$LLZ" self-update --repo "$REPO" --dry-run 2>&1 | grep -qiE 'updating llz|already on'; then
      pass "self-update --dry-run resolves the latest release"
    else
      fail "self-update --dry-run did not report a resolved target"
    fi
  fi
fi

# ── result ───────────────────────────────────────────────────────────────────
echo
if [[ "$FAILED" -ne 0 ]]; then
  echo "::error::llz-functional FAILED"
  exit 1
fi
echo "llz-functional OK"
