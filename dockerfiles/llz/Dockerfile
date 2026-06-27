# Slim, in-cluster runtime image for the `llz` CLI.
#
# Unlike ci-terraform/ci-kubernetes/devcontainer (fat CI/workstation images that
# also bake tofu/kubectl/helm/etc.), this image carries ONLY the static llz
# binary on a distroless base — for workloads that run `llz` IN the cluster
# (e.g. the Linode credential rotator CronJob, and the volume-labeler once it
# moves off the alpine/k8s shell script). Minimal attack surface matters: these
# pods mount a Linode API token + an OpenBao auth path.
#
# Built like the other images from the `tools-src` build context
# (build-images.yml passes `--build-context tools-src=tools`); main.version is
# stamped from LLZ_VERSION exactly as the CI images + llz-release.yml do.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG LLZ_VERSION=dev
WORKDIR /src
COPY --from=tools-src go.mod go.sum ./
RUN go mod download
COPY --from=tools-src . .
# CGO_ENABLED=0 → a fully static binary that runs on distroless/static.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${LLZ_VERSION}" -o /out/llz ./cmd/llz

# distroless/static: no shell, no package manager — just the binary + a CA bundle
# (for HTTPS to the Linode API / GitHub) and /etc/passwd with `nonroot` (65532).
# The OpenBao private CA is NOT in this bundle; in-cluster callers mount it and
# point llz at it explicitly.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llz /usr/local/bin/llz
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/llz"]
