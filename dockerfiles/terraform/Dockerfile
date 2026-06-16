# Multi-stage: build the CI Go binary from the tools module, supplied as the
# `tools-src` build context (build-images.yml passes `--build-context
# tools-src=tools`). Baked into the runtime image below so CI jobs get it on
# PATH with no runtime `go build` — and so this image carries no Go toolchain.
#   llz               — the CI/credential CLI (`llz ci …`, `llz credentials …`;
#                       includes the lke-admin rotation and the PAT/OBJ-key
#                       audit formerly shipped as standalone binaries).
# main.version is stamped from LLZ_VERSION exactly as llz-release.yml /
# the devcontainer image do, so a baked `llz version` is traceable to its build.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS gotools
ARG TARGETOS
ARG TARGETARCH
ARG LLZ_VERSION=dev
WORKDIR /src
COPY --from=tools-src go.mod go.sum ./
RUN go mod download
COPY --from=tools-src . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${LLZ_VERSION}" -o /out/llz ./cmd/llz

FROM debian:bookworm-slim

ARG TARGETARCH
ARG UV_VERSION=0.6.14
ARG TF_VERSION=1.9.8
ARG TFLINT_VERSION=0.53.0
ARG HELM_VERSION=3.17.3
ARG KUBECTL_VERSION=1.31.0
ARG KUSTOMIZE_VERSION=5.4.3

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

# Terraform (checksum-verified)
RUN set -eux; \
    ZIP="terraform_${TF_VERSION}_linux_${TARGETARCH}.zip"; \
    BASE="https://releases.hashicorp.com/terraform/${TF_VERSION}"; \
    curl -sSfL "${BASE}/${ZIP}" -o "/tmp/${ZIP}"; \
    curl -sSfL "${BASE}/terraform_${TF_VERSION}_SHA256SUMS" -o /tmp/SHA256SUMS; \
    grep "${ZIP}" /tmp/SHA256SUMS | (cd /tmp && sha256sum --check --status); \
    unzip -q -o "/tmp/${ZIP}" terraform -d /usr/local/bin; \
    chmod +x /usr/local/bin/terraform; \
    rm -f "/tmp/${ZIP}" /tmp/SHA256SUMS

# tflint
RUN set -eux; \
    TARBALL="tflint_linux_${TARGETARCH}.zip"; \
    BASE="https://github.com/terraform-linters/tflint/releases/download/v${TFLINT_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    curl -fsSLO "${BASE}/checksums.txt"; \
    grep "${TARBALL}" checksums.txt | sha256sum -c; \
    unzip -oq "${TARBALL}" -d /usr/local/bin; \
    rm "${TARBALL}" checksums.txt

# Helm
RUN set -eux; \
    TARBALL="helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl -fsSLO "https://get.helm.sh/${TARBALL}"; \
    curl -fsSLO "https://get.helm.sh/${TARBALL}.sha256sum"; \
    sha256sum -c "${TARBALL}.sha256sum"; \
    tar -xzf "${TARBALL}" --strip-components=1 -C /usr/local/bin "linux-${TARGETARCH}/helm"; \
    rm "${TARBALL}" "${TARBALL}.sha256sum"

# kubectl
RUN set -eux; \
    curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    curl -fsSLo /tmp/kubectl.sha256 \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256"; \
    echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c; \
    chmod +x /usr/local/bin/kubectl; \
    rm /tmp/kubectl.sha256

# kustomize
RUN set -eux; \
    TARBALL="kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    BASE="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    curl -fsSLO "${BASE}/checksums.txt"; \
    grep "${TARBALL}" checksums.txt | sha256sum -c; \
    tar -xzf "${TARBALL}" -C /usr/local/bin kustomize; \
    rm "${TARBALL}" checksums.txt

# uv (fast Python package installer)
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) UV_ARCH="x86_64-unknown-linux-gnu" ;; \
      arm64) UV_ARCH="aarch64-unknown-linux-gnu" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/uv.tar.gz \
      "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${UV_ARCH}.tar.gz"; \
    tar -xzf /tmp/uv.tar.gz -C /usr/local/bin --strip-components=1 \
      "uv-${UV_ARCH}/uv" "uv-${UV_ARCH}/uvx"; \
    rm /tmp/uv.tar.gz

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

# CI Go binary, built from the tools module in the `gotools` stage above and
# baked onto PATH — so CI jobs call `llz` with no runtime `go build`. This is
# why the image ships NO Go toolchain: the binary is prebuilt here. It tracks
# the image tag (vars.TF_IMAGE), not the workflow template-ref — keep TF_IMAGE
# in step with the template release the instance pins.
COPY --from=gotools /out/llz               /usr/local/bin/llz
