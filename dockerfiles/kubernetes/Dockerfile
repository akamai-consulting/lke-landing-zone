# Multi-stage: bake `llz` from the Go tools module, supplied as the `tools-src`
# build context (build-images.yml passes `--build-context tools-src=tools`) —
# same pattern as dockerfiles/terraform. `make lint-k8s` runs in this image and
# its externalsecret-paths-check target calls `llz ci externalsecret-paths`, so
# the CLI must be on PATH with no runtime `go build` (the image ships no Go
# toolchain). LLZ_VERSION stamps main.version for traceability.
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
ARG HELM_VERSION=3.17.3
ARG KUBECTL_VERSION=1.31.0
ARG KUBE_LINTER_VERSION=0.8.3
ARG KUBECONFORM_VERSION=0.7.0
ARG ARGO_VERSION=3.6.5
ARG PROM_VERSION=3.4.0
ARG SHELLCHECK_VERSION=v0.10.0
ARG YQ_VERSION=4.44.3

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

# promtool (extracted from prometheus release)
RUN set -eux; \
    TARBALL="prometheus-${PROM_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    curl -fsSLo /tmp/prometheus.tar.gz \
      "https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/${TARBALL}"; \
    tar -xzf /tmp/prometheus.tar.gz -C /tmp \
      "prometheus-${PROM_VERSION}.linux-${TARGETARCH}/promtool"; \
    mv "/tmp/prometheus-${PROM_VERSION}.linux-${TARGETARCH}/promtool" /usr/local/bin/promtool; \
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
      | tar -xJ --strip-components=1 -C /usr/local/bin \
          "shellcheck-${SHELLCHECK_VERSION}/shellcheck"

# yq (YAML processor — used inline in dry-run pipelines)
RUN set -eux; \
    curl -fsSLo /usr/local/bin/yq \
      "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${TARGETARCH}"; \
    chmod +x /usr/local/bin/yq

# llz — baked from the tools module above so `make lint-k8s` (externalsecret
# paths check) calls it with no runtime go build.
COPY --from=gotools /out/llz /usr/local/bin/llz
