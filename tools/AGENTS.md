# tools/ — Native Go utilities

This directory contains native, host-side platform tooling shipped by the
landing-zone template. It is a single Go module (`go.mod`).

## Layout

- `cmd/llz/` — adopter-facing front-end CLI. Orchestrates the existing setup /
  upgrade flow (shells out to `copier`, `gh`, `kubectl`, and `scripts/*.sh`); it
  does not reimplement them. Its one original piece is the token wizard
  (`wizard.go`) that requests every credential and prints a pre-filled creation
  link. Cloud-mutating subcommands (`secrets push`, `build`, `bootstrap`) execute
  only with `--yes`. Distributed as prebuilt release binaries
  attached to the umbrella release (`.github/workflows/llz-release.yml`, on the
  bare `vX.Y.Z` release event). See docs/quickstart.md.
  `llz credentials` owns the mutating half of the shared Linode credential
  lifecycles (formerly the standalone `linode-pat-rotator` /
  `linode-obj-key-rotator` binaries): `pat create|revoke-old` for the
  `LINODE_API_TOKEN` PAT (90-day PAT policy, grace-by-age drain) and `obj-key
  create|revoke-old` for the 120-day TF-state Object Storage key SLA
  (keep-newest-N drain — the OBJ keys API exposes no `created` time). Built and
  exec'd by the `linode-credentials` composite action. The former standalone
  `secret-rotation` binary is folded in too, as `llz credentials lke-admin
  rotate`. (`linode-cred-audit` became `llz ci cred-audit` and has since been
  RETIRED in turn — its measurement lives in `llz ci token-inventory` and its
  reporting in `llz ci alert-eval`.)
- `internal/linode/` — the shared, minimal Linode API client (LKE control-plane
  ACL; profile-token / OBJ-key CRUD and the lke-admin delete-kubeconfig
  rotation), plus chrono-free Linode timestamp helpers, used by the rotation
  tools.
- `internal/cli/` — small shared argument-parsing / env-default / JSON-record
  helpers used by the rotation commands.

Run from the module root (`tools/`):

```bash
go build ./...
go test ./...
go vet ./...
gofmt -l .            # must print nothing
```

## Conventions

- **Standard library first.** Third-party dependencies are kept minimal
  (`spf13/cobra` for the CLI, `sigs.k8s.io/yaml` for YAML), reaching the
  Kubernetes API with a hand-rolled in-cluster REST client rather than client-go.
- Binaries build fully static (`CGO_ENABLED=0`).
- **`ci assert-*` verbs are the e2e gates** — the layer that catches behavior no
  static check can see. Structure every new one the same way: the judgement is a
  **pure function over parsed input** (testable without a cluster), and the
  transport — `kubectl`, port-forward, an API client — sits behind a **package-var
  seam** a test replaces (`withPrometheus` / `withLoki` in `prom_query.go` /
  `loki_query.go`). `ci_assert_scrape.go` and `ci_assert_openbao_audit.go` are the
  models. Unit-test the pure evaluator *and* the fail-closed arms: empty result,
  malformed response, unreachable endpoint. A gate must never report success
  having examined nothing. When two components in this module share a rule (a
  label format, a path layout, a truncation limit), test the coupling by calling
  **both sides' real functions** — never by restating the rule in the test, which
  passes happily while the shipped consumer goes blind. See
  [docs/e2e-gates.md](../docs/e2e-gates.md).
- Never add `Co-Authored-By` to commits.
- Do not commit secrets or keypairs (`*.pem`, `*.der`, `*.key`) — `.gitignore`
  covers these and the pre-commit hook enforces it.
- Do not make changes without explicit user approval.
