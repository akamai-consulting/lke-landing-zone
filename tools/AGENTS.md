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
  `secret-rotation` and `linode-cred-audit` binaries are folded in too, as
  `llz credentials lke-admin rotate` and `llz ci cred-audit`.
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
- Never add `Co-Authored-By` to commits.
- Do not commit secrets or keypairs (`*.pem`, `*.der`, `*.key`) — `.gitignore`
  covers these and the pre-commit hook enforces it.
- Do not make changes without explicit user approval.
