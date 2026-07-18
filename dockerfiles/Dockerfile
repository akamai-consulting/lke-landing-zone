# Every container image this repo publishes, as one multi-stage build.
#
# Targets (build with `--target <name>`, see .github/workflows/build-images.yml):
#   ci-terraform   — Terraform/IaC CI image (ghcr.io/<org>/ci-terraform)
#   ci-kubernetes  — Kubernetes lint/validate CI image (ghcr.io/<org>/ci-kubernetes)
#   devcontainer   — adopter workstation image (ghcr.io/<org>/devcontainer)
#   llz            — slim distroless in-cluster image (ghcr.io/<org>/llz)
#
# WHY ONE FILE. These were four Dockerfiles that each re-implemented the same
# work: an identical Go build stage (4 copies), and the same download-verify-
# extract blocks for helm/kubectl/terraform/… (Helm and kubectl were triplicated).
# Worse, every tool version was pinned in 2-3 separate `ARG`s plus, for terraform
# and kubectl, a hardcoded image tag in build-images.yml — with a "kept in
# lockstep" comment and NOTHING enforcing it. A missed copy did not fail; it
# silently shipped a devcontainer whose helm differed from CI's.
#
# Now: `gotools` builds llz once, `toolbox` downloads every static CLI once from
# a single set of version ARGs, and each image COPYs the binaries it needs. A
# version bump is one edit, and the images cannot disagree.
#
# The tool ARGs in `toolbox` are the single source of truth for what goes INTO
# the images. Two version-named image TAGS still restate a pin outside this file
# and must be bumped with it — `ci-terraform:<TF_VERSION>` and
# `ci-kubernetes:<KUBECTL_VERSION>` in .github/workflows/build-images.yml, and the
# same two literals as the `vars.TF_IMAGE`/`vars.KUBE_IMAGE` fallbacks in
# .github/workflows/lint.yml. Nothing enforces that yet; a guard verb is the
# obvious follow-up.

# ── gotools: build the llz CLI once, for every image that bakes it ────────────
# The Go tools module arrives as the `tools-src` build context (build-images.yml
# passes `--build-context tools-src=tools`) so the primary context stays small.
# Cross-compiles from BUILDPLATFORM via GOOS/GOARCH — no QEMU for the compile.
# main.version is stamped from LLZ_VERSION exactly as llz-release.yml does, so a
# baked `llz version` is traceable to the build that produced it.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS gotools
ARG TARGETOS
ARG TARGETARCH
ARG LLZ_VERSION=dev
WORKDIR /src
COPY --from=tools-src go.mod go.sum ./
RUN go mod download
COPY --from=tools-src . .
# CGO_ENABLED=0 → a fully static binary, required by the distroless `llz` image
# and harmless for the others.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${LLZ_VERSION}" -o /out/llz ./cmd/llz

# ── toolbox: download + checksum-verify every third-party CLI, once ───────────
# Everything here is a statically linked binary (Go, or Haskell for shellcheck),
# so the final images can COPY them across different bases — debian:bookworm-slim
# for CI, mcr devcontainers/base:bookworm for the workstation image. Tools that
# are NOT a single static binary (gh via apt, and the Python CLIs via uv) stay in
# the image that needs them, further down.
#
# Runs on the target platform, as these installs always have: the release assets
# are selected by TARGETARCH and only downloaded/verified here, never executed.
FROM debian:bookworm-slim AS toolbox
ARG TARGETARCH

# ── the single source of truth for tool versions ──
ARG TF_VERSION=1.9.8
ARG TFLINT_VERSION=0.53.0
ARG HELM_VERSION=3.17.3
ARG KUBECTL_VERSION=1.31.0
ARG KUSTOMIZE_VERSION=5.4.3
ARG KUBE_LINTER_VERSION=0.8.3
ARG KUBECONFORM_VERSION=0.7.0
ARG ARGO_VERSION=3.6.5
ARG PROM_VERSION=3.4.0
ARG SHELLCHECK_VERSION=v0.10.0
ARG YQ_VERSION=4.44.3
ARG UV_VERSION=0.6.14
ARG BAO_VERSION=2.5.4
ARG ACTIONLINT_VERSION=1.7.7

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      unzip \
      xz-utils \
      tar \
      gzip \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /out/bin

# Terraform (checksum-verified)
RUN set -eux; \
    ZIP="terraform_${TF_VERSION}_linux_${TARGETARCH}.zip"; \
    BASE="https://releases.hashicorp.com/terraform/${TF_VERSION}"; \
    curl -sSfL "${BASE}/${ZIP}" -o "/tmp/${ZIP}"; \
    curl -sSfL "${BASE}/terraform_${TF_VERSION}_SHA256SUMS" -o /tmp/SHA256SUMS; \
    grep "${ZIP}" /tmp/SHA256SUMS | (cd /tmp && sha256sum --check --status); \
    unzip -q -o "/tmp/${ZIP}" terraform -d /out/bin; \
    chmod +x /out/bin/terraform; \
    rm -f "/tmp/${ZIP}" /tmp/SHA256SUMS

# tflint
RUN set -eux; \
    TARBALL="tflint_linux_${TARGETARCH}.zip"; \
    BASE="https://github.com/terraform-linters/tflint/releases/download/v${TFLINT_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    curl -fsSLO "${BASE}/checksums.txt"; \
    grep "${TARBALL}" checksums.txt | sha256sum -c; \
    unzip -oq "${TARBALL}" -d /out/bin; \
    rm "${TARBALL}" checksums.txt

# Helm
RUN set -eux; \
    TARBALL="helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl -fsSLO "https://get.helm.sh/${TARBALL}"; \
    curl -fsSLO "https://get.helm.sh/${TARBALL}.sha256sum"; \
    sha256sum -c "${TARBALL}.sha256sum"; \
    tar -xzf "${TARBALL}" --strip-components=1 -C /out/bin "linux-${TARGETARCH}/helm"; \
    rm "${TARBALL}" "${TARBALL}.sha256sum"

# kubectl
RUN set -eux; \
    curl -fsSLo /out/bin/kubectl \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    curl -fsSLo /tmp/kubectl.sha256 \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256"; \
    echo "$(cat /tmp/kubectl.sha256)  /out/bin/kubectl" | sha256sum -c; \
    chmod +x /out/bin/kubectl; \
    rm /tmp/kubectl.sha256

# kustomize
RUN set -eux; \
    TARBALL="kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    BASE="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    curl -fsSLO "${BASE}/checksums.txt"; \
    grep "${TARBALL}" checksums.txt | sha256sum -c; \
    tar -xzf "${TARBALL}" -C /out/bin kustomize; \
    rm "${TARBALL}" checksums.txt

# kube-linter (different tarball name for arm64)
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) KL_TARBALL="kube-linter-linux.tar.gz" ;; \
      arm64) KL_TARBALL="kube-linter-linux_arm64.tar.gz" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    BASE="https://github.com/stackrox/kube-linter/releases/download/v${KUBE_LINTER_VERSION}"; \
    curl -fsSLO "${BASE}/${KL_TARBALL}"; \
    tar -xzf "${KL_TARBALL}" -C /out/bin kube-linter; \
    rm "${KL_TARBALL}"

# kubeconform
RUN set -eux; \
    TARBALL="kubeconform-linux-${TARGETARCH}.tar.gz"; \
    BASE="https://github.com/yannh/kubeconform/releases/download/v${KUBECONFORM_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    tar -xzf "${TARBALL}" -C /out/bin kubeconform; \
    rm "${TARBALL}"

# argo CLI
RUN set -eux; \
    curl -fsSLo /tmp/argo.gz \
      "https://github.com/argoproj/argo-workflows/releases/download/v${ARGO_VERSION}/argo-linux-${TARGETARCH}.gz"; \
    gunzip -c /tmp/argo.gz > /out/bin/argo; \
    chmod +x /out/bin/argo; \
    rm /tmp/argo.gz

# promtool (extracted from the prometheus release)
RUN set -eux; \
    TARBALL="prometheus-${PROM_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    curl -fsSLo /tmp/prometheus.tar.gz \
      "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/${TARBALL}"; \
    tar -xzf /tmp/prometheus.tar.gz -C /tmp \
      "prometheus-${PROM_VERSION}.linux-${TARGETARCH}/promtool"; \
    mv "/tmp/prometheus-${PROM_VERSION}.linux-${TARGETARCH}/promtool" /out/bin/promtool; \
    rm -rf /tmp/prometheus.tar.gz \
           "/tmp/prometheus-${PROM_VERSION}.linux-${TARGETARCH}"

# shellcheck (different arch names from TARGETARCH)
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) SC_ARCH="x86_64" ;; \
      arm64) SC_ARCH="aarch64" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    curl -sSL "https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION}.linux.${SC_ARCH}.tar.xz" \
      | tar -xJ --strip-components=1 -C /out/bin \
          "shellcheck-${SHELLCHECK_VERSION}/shellcheck"

# yq (YAML processor — apl-values/manifest editing + dry-run pipelines)
RUN set -eux; \
    curl -fsSLo /out/bin/yq \
      "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${TARGETARCH}"; \
    chmod +x /out/bin/yq

# uv + uvx (fast Python package installer; lands the Python CLIs in the images
# that need them)
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) UV_ARCH="x86_64-unknown-linux-gnu" ;; \
      arm64) UV_ARCH="aarch64-unknown-linux-gnu" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/uv.tar.gz \
      "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${UV_ARCH}.tar.gz"; \
    tar -xzf /tmp/uv.tar.gz -C /out/bin --strip-components=1 \
      "uv-${UV_ARCH}/uv" "uv-${UV_ARCH}/uvx"; \
    rm /tmp/uv.tar.gz

# bao (OpenBao CLI — `llz doctor` checks for it; used to inspect OpenBao). The
# release assets use goreleaser naming (capital `Linux`, `x86_64` for amd64) and
# a per-OS checksums file. The grep is end-anchored: the tarball name is a prefix
# of the sibling `<tarball>.sbom.json` line, so an unanchored match would feed
# sha256sum -c a checksum for a file we never downloaded and fail.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) BAO_ARCH="x86_64" ;; \
      arm64) BAO_ARCH="arm64" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    TARBALL="bao_${BAO_VERSION}_Linux_${BAO_ARCH}.tar.gz"; \
    BASE="https://github.com/openbao/openbao/releases/download/v${BAO_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    curl -fsSLO "${BASE}/checksums-linux.txt"; \
    grep " ${TARBALL}\$" checksums-linux.txt | sha256sum -c; \
    tar -xzf "${TARBALL}" -C /out/bin bao; \
    rm "${TARBALL}" checksums-linux.txt

# actionlint (official installer pins the version + arch)
RUN set -eux; \
    curl -fsSL https://raw.githubusercontent.com/rhysd/actionlint/main/scripts/download-actionlint.bash \
      | bash -s -- "${ACTIONLINT_VERSION}" /out/bin

# ── ci-terraform ──────────────────────────────────────────────────────────────
# Terraform/IaC CI image. Ships NO Go toolchain: `llz` is prebuilt in `gotools`
# and baked onto PATH, so CI jobs never run a runtime `go build`. Its tag tracks
# vars.TF_IMAGE, not the workflow template-ref — keep TF_IMAGE in step with the
# template release the instance pins.
FROM debian:bookworm-slim AS ci-terraform

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      unzip \
      git \
      ssh \
      make \
      gpg \
      gpg-agent \
      python3 \
      jq \
    && rm -rf /var/lib/apt/lists/*

# In container jobs the workspace is bind-mounted with the runner user's
# ownership, which differs from the UID git runs as inside this container, so
# git trips its "detected dubious ownership" guard (>= 2.35.2). Trust every path
# system-wide for this ephemeral CI user so `terraform init` module clones and
# any `git` step proceed — formerly set ad hoc by the gh-install steps and
# git-auth.sh; baked here so it holds regardless of step order.
RUN git config --system --add safe.directory '*'

COPY --from=toolbox /out/bin/terraform /usr/local/bin/terraform
COPY --from=toolbox /out/bin/tflint    /usr/local/bin/tflint
COPY --from=toolbox /out/bin/helm      /usr/local/bin/helm
COPY --from=toolbox /out/bin/kubectl   /usr/local/bin/kubectl
COPY --from=toolbox /out/bin/kustomize /usr/local/bin/kustomize
COPY --from=toolbox /out/bin/uv        /usr/local/bin/uv
COPY --from=toolbox /out/bin/uvx       /usr/local/bin/uvx

# checkov (Python IaC security scanner)
RUN uv pip install --system --break-system-packages --no-cache "checkov>=3.2.0,<4.0.0"

# GitHub CLI (apt repo — arch + updates handled by apt). CI jobs use `gh secret
# set` to persist unseal keys + rotated credentials; baked in so jobs need no
# runtime install step.
RUN set -eux; \
    mkdir -p -m 755 /etc/apt/keyrings; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update -qq; \
    apt-get install -y --no-install-recommends gh; \
    rm -rf /var/lib/apt/lists/*

COPY --from=gotools /out/llz /usr/local/bin/llz

# ── ci-kubernetes ─────────────────────────────────────────────────────────────
# Kubernetes lint/validate CI image. `make lint-k8s` runs in here and its
# externalsecret-paths-check target calls `llz ci externalsecret-paths`, so the
# CLI must be on PATH with no runtime `go build` (this image ships no Go
# toolchain either).
FROM debian:bookworm-slim AS ci-kubernetes

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      ssh \
      make \
      jq \
      xz-utils \
    && rm -rf /var/lib/apt/lists/*

COPY --from=toolbox /out/bin/helm        /usr/local/bin/helm
COPY --from=toolbox /out/bin/kubectl     /usr/local/bin/kubectl
COPY --from=toolbox /out/bin/kube-linter /usr/local/bin/kube-linter
COPY --from=toolbox /out/bin/kubeconform /usr/local/bin/kubeconform
COPY --from=toolbox /out/bin/argo        /usr/local/bin/argo
COPY --from=toolbox /out/bin/promtool    /usr/local/bin/promtool
COPY --from=toolbox /out/bin/shellcheck  /usr/local/bin/shellcheck
COPY --from=toolbox /out/bin/yq          /usr/local/bin/yq

COPY --from=gotools /out/llz /usr/local/bin/llz

# ── devcontainer ──────────────────────────────────────────────────────────────
# The "adopter workstation in a box": every CLI `llz doctor` checks (terraform,
# kubectl, helm, bao, copier, gh, linode-cli, jq, ssh-keygen) plus the lint stack
# (tflint, kustomize, kube-linter, kubeconform, checkov, actionlint, shellcheck,
# yq, argo), so "Reopen in Container" gives a full toolchain with nothing
# installed on the host. The instance scaffold's .devcontainer/devcontainer.json
# references the multi-arch image this builds (native on Intel/AMD and Apple
# Silicon).
#
# Tool versions match the CI images BY CONSTRUCTION — the binaries below are
# copied from the same `toolbox` stage ci-terraform/ci-kubernetes copy from, so
# local checks cannot drift from CI.
FROM mcr.microsoft.com/devcontainers/base:bookworm AS devcontainer

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      unzip \
      git \
      ssh \
      openssh-client \
      make \
      gpg \
      gpg-agent \
      jq \
      python3 \
      python3-pip \
    && rm -rf /var/lib/apt/lists/*

COPY --from=toolbox /out/bin/terraform   /usr/local/bin/terraform
COPY --from=toolbox /out/bin/tflint      /usr/local/bin/tflint
COPY --from=toolbox /out/bin/helm        /usr/local/bin/helm
COPY --from=toolbox /out/bin/kubectl     /usr/local/bin/kubectl
COPY --from=toolbox /out/bin/kustomize   /usr/local/bin/kustomize
COPY --from=toolbox /out/bin/kube-linter /usr/local/bin/kube-linter
COPY --from=toolbox /out/bin/kubeconform /usr/local/bin/kubeconform
COPY --from=toolbox /out/bin/argo        /usr/local/bin/argo
COPY --from=toolbox /out/bin/yq          /usr/local/bin/yq
COPY --from=toolbox /out/bin/shellcheck  /usr/local/bin/shellcheck
COPY --from=toolbox /out/bin/bao         /usr/local/bin/bao
COPY --from=toolbox /out/bin/actionlint  /usr/local/bin/actionlint
COPY --from=toolbox /out/bin/uv          /usr/local/bin/uv
COPY --from=toolbox /out/bin/uvx         /usr/local/bin/uvx

# GitHub CLI (apt repo — arch + updates handled by apt)
RUN set -eux; \
    mkdir -p -m 755 /etc/apt/keyrings; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update -qq; \
    apt-get install -y --no-install-recommends gh; \
    rm -rf /var/lib/apt/lists/*

# Python CLIs: copier (scaffold/update), linode-cli (the wizard + obj-cluster
# lookup), checkov (IaC security scan). Installed into the system interpreter.
RUN uv pip install --system --break-system-packages --no-cache \
      "copier>=9.4,<10" \
      "linode-cli>=5.0" \
      "checkov>=3.2.0,<4.0.0"

# llz adopter CLI — drives the whole flow (`llz doctor`, `llz tokens`,
# `llz build`, `llz upgrade`). Baked in so the container has it OOTB; adopters
# can install a different release over it.
COPY --from=gotools /out/llz /usr/local/bin/llz

# Drop back to the base image's non-root user for the interactive session.
USER vscode

# ── llz ───────────────────────────────────────────────────────────────────────
# Slim, in-cluster runtime image carrying ONLY the static llz binary — for
# workloads that run `llz` IN the cluster (the Linode credential rotator CronJob,
# and the volume-labeler once it moves off the alpine/k8s shell script). Minimal
# attack surface matters: these pods mount a Linode API token + an OpenBao auth
# path. Note this target depends on `gotools` only, never `toolbox`.
#
# distroless/static: no shell, no package manager — just the binary + a CA bundle
# (for HTTPS to the Linode API / GitHub) and /etc/passwd with `nonroot` (65532).
# The OpenBao private CA is NOT in this bundle; in-cluster callers mount it and
# point llz at it explicitly.
FROM gcr.io/distroless/static-debian12:nonroot AS llz
COPY --from=gotools /out/llz /usr/local/bin/llz
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/llz"]
