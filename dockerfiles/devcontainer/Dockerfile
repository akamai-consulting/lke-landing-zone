# Dev Container image for working on an LLZ instance repo.
#
# This is the "adopter workstation in a box": it bundles every CLI `llz doctor`
# checks (terraform, kubectl, helm, bao, copier, gh, linode-cli, jq, ssh-keygen)
# plus the lint/validate stack (tflint, kustomize, kube-linter, kubeconform,
# checkov, actionlint, shellcheck, yq, argo) so "Reopen in Container" gives an
# adopter the full toolchain without installing anything on the host. The
# instance scaffold's .devcontainer/devcontainer.json references the multi-arch
# image this builds (amd64 + arm64 — native on Intel/AMD and Apple Silicon).
#
# Tool versions are kept in lockstep with the ci-terraform / ci-kubernetes images
# (dockerfiles/{terraform,kubernetes}/Dockerfile) so local checks match CI.
# Built and pushed by .github/workflows/build-images.yml.

# Build the llz adopter CLI from the Go tools module — supplied as the
# `tools-src` build context (build-images.yml passes `--build-context
# tools-src=tools`), the same pattern ci-terraform uses for `llz`. This
# ships a working `llz` OOTB without a private-repo release download, and the
# binary always matches the template commit that built the image. main.version is
# stamped from LLZ_VERSION exactly as .github/workflows/llz-release.yml does.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS llz
ARG TARGETOS
ARG TARGETARCH
ARG LLZ_VERSION=dev
WORKDIR /src
COPY --from=tools-src go.mod go.sum ./
RUN go mod download
COPY --from=tools-src . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${LLZ_VERSION}" \
      -o /out/llz ./cmd/llz

FROM mcr.microsoft.com/devcontainers/base:bookworm

# buildx populates TARGETARCH (amd64 | arm64) per --platform; the tool installs
# below select the matching release asset from it.
ARG TARGETARCH

ARG TF_VERSION=1.9.8
ARG TFLINT_VERSION=0.53.0
ARG HELM_VERSION=3.17.3
ARG KUBECTL_VERSION=1.31.0
ARG KUSTOMIZE_VERSION=5.4.3
ARG KUBE_LINTER_VERSION=0.8.3
ARG KUBECONFORM_VERSION=0.7.0
ARG ARGO_VERSION=3.6.5
ARG YQ_VERSION=4.44.3
ARG SHELLCHECK_VERSION=v0.10.0
ARG UV_VERSION=0.6.14
ARG BAO_VERSION=2.5.4
ARG ACTIONLINT_VERSION=1.7.7
ARG GLAB_VERSION=1.102.0

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

# kube-linter (different tarball name for arm64)
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) KL_TARBALL="kube-linter-linux.tar.gz" ;; \
      arm64) KL_TARBALL="kube-linter-linux_arm64.tar.gz" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    BASE="https://github.com/stackrox/kube-linter/releases/download/v${KUBE_LINTER_VERSION}"; \
    curl -fsSLO "${BASE}/${KL_TARBALL}"; \
    tar -xzf "${KL_TARBALL}" -C /usr/local/bin kube-linter; \
    rm "${KL_TARBALL}"

# kubeconform
RUN set -eux; \
    TARBALL="kubeconform-linux-${TARGETARCH}.tar.gz"; \
    BASE="https://github.com/yannh/kubeconform/releases/download/v${KUBECONFORM_VERSION}"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    tar -xzf "${TARBALL}" -C /usr/local/bin kubeconform; \
    rm "${TARBALL}"

# argo CLI
RUN set -eux; \
    curl -fsSLo /tmp/argo.gz \
      "https://github.com/argoproj/argo-workflows/releases/download/v${ARGO_VERSION}/argo-linux-${TARGETARCH}.gz"; \
    gunzip -c /tmp/argo.gz > /usr/local/bin/argo; \
    chmod +x /usr/local/bin/argo; \
    rm /tmp/argo.gz

# yq (YAML processor — apl-values/manifest editing + dry-run pipelines)
RUN set -eux; \
    curl -fsSLo /usr/local/bin/yq \
      "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${TARGETARCH}"; \
    chmod +x /usr/local/bin/yq

# shellcheck (different arch names from TARGETARCH)
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) SC_ARCH="x86_64" ;; \
      arm64) SC_ARCH="aarch64" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac; \
    curl -sSL "https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION}.linux.${SC_ARCH}.tar.xz" \
      | tar -xJ --strip-components=1 -C /usr/local/bin \
          "shellcheck-${SHELLCHECK_VERSION}/shellcheck"

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
    tar -xzf "${TARBALL}" -C /usr/local/bin bao; \
    rm "${TARBALL}" checksums-linux.txt

# actionlint (official installer pins the version + arch into /usr/local/bin)
RUN set -eux; \
    curl -fsSL https://raw.githubusercontent.com/rhysd/actionlint/main/scripts/download-actionlint.bash \
      | bash -s -- "${ACTIONLINT_VERSION}" /usr/local/bin

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

# GitLab CLI (checksum-verified tarball — no official apt repo, so mirror the
# kustomize pattern rather than the gh apt install). The GitLab forge backend
# (LLZ_FORGE=gitlab) shells out to `glab` as the default backend uses `gh`, and
# `llz doctor` checks `glab auth status` when the flavor is GitLab — ship it so
# the workstation is complete. GitLab support is still best-effort/untested.
RUN set -eux; \
    TARBALL="glab_${GLAB_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    BASE="https://gitlab.com/gitlab-org/cli/-/releases/v${GLAB_VERSION}/downloads"; \
    curl -fsSLO "${BASE}/${TARBALL}"; \
    curl -fsSLO "${BASE}/checksums.txt"; \
    grep "${TARBALL}" checksums.txt | sha256sum -c; \
    tar -xzf "${TARBALL}" -C /usr/local/bin --strip-components=1 bin/glab; \
    rm "${TARBALL}" checksums.txt

# uv (fast Python package installer — used to land the Python CLIs below)
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

# Python CLIs: copier (scaffold/update), linode-cli (the wizard + obj-cluster
# lookup), checkov (IaC security scan). Installed into the system interpreter.
RUN uv pip install --system --break-system-packages --no-cache \
      "copier>=9.4,<10" \
      "linode-cli>=5.0" \
      "checkov>=3.2.0,<4.0.0"

# llz adopter CLI (built from the tools module above) — drives the whole flow
# (`llz doctor`, `llz tokens`, `llz build`, `llz upgrade`). Baked in so the
# container has it OOTB; adopters can install a different release over it.
COPY --from=llz /out/llz /usr/local/bin/llz

# Drop back to the base image's non-root user for the interactive session.
USER vscode
