# Slim, in-cluster runtime image: the `llz` CLI + kubectl, nonroot/distroless —
# for IN-CLUSTER CronJobs that call `llz ci …` (the Linode credential rotator,
# the harbor-robot provisioner, and the volume-tag-reconciler's `llz ci
# reconcile-volume-tags`). kubectl is bundled for the reconciler's
# `kubectl get pv/storageclass`; the API-only rotator/harbor jobs don't invoke it.
#
# Distinct from ci-kubernetes (a fat CI image with helm/argo/trivy/etc. and no
# nonroot USER): this one is distroless + nonroot so the pod keeps runAsNonRoot +
# readOnlyRootFilesystem — the security posture a pod that mounts a Linode API
# token + an OpenBao auth path wants. `llz` is built CGO_ENABLED=0 (static) and
# kubectl is statically linked, so both run on distroless.
#
# LLZ_VERSION stamps main.version for traceability, exactly like the other images.
# Built with `--build-context tools-src=tools` so the Go module is available to
# the build stage without a separate builder image.

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

# kubectl — pinned + sha-verified (mirrors dockerfiles/kubernetes). Only used for
# `get pv/nodes -o json`, so exact version is not load-bearing, but keep it within
# kubectl's ±1-minor skew of the LKE control plane (bump when clusters move up —
# this trails the old alpine/k8s 1.32.x that shipped in the ConfigMap image).
FROM debian:bookworm-slim AS kubectl
ARG TARGETARCH
ARG KUBECTL_VERSION=1.31.0
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl ca-certificates; \
    curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    curl -fsSLo /tmp/kubectl.sha256 \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256"; \
    echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c; \
    chmod +x /usr/local/bin/kubectl; \
    rm /tmp/kubectl.sha256

# distroless base: glibc + ca-certs (kubectl + llz HTTPS to api.linode.com), and
# the :nonroot tag runs as uid 65532 — matching the CronJob's runAsUser.
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=gotools /out/llz        /usr/local/bin/llz
COPY --from=kubectl /usr/local/bin/kubectl /usr/local/bin/kubectl
USER nonroot
ENTRYPOINT ["llz"]
