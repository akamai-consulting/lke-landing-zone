#!/usr/bin/env bash
# Generate per-image CycloneDX SBOMs for every container `image:` referenced
# under kubernetes/. One output file per unique image, written to
# sbom/sbom-k8s-<sanitized>.json so the release.yml `gh release upload sbom/*.json`
# picks them up alongside the Go + Terraform SBOMs.
#
# Skips images that can't be pulled or scanned (auth, network, missing tag) with
# a warning so partial coverage doesn't fail the release. Authenticate to
# private registries via ~/.docker/config.json (set DOCKER_CONFIG) — see
# release.yml for the CI auth setup.
#
# Limitations:
#   * Only matches literal `image: <ref>` YAML. Helm-templated or kustomize-
#     transformed image fields that don't appear in the rendered YAML on disk
#     are missed.
#   * Skips templated values (e.g. `${IMAGE_TAG}`) — those aren't resolvable
#     at scan time.
set -euo pipefail

ROOT="${SBOM_K8S_SOURCE_DIR:-kubernetes}"
OUT_DIR="${SBOM_OUT_DIR:-sbom}"

if ! command -v trivy >/dev/null 2>&1; then
    echo "trivy not found on PATH — install via \`make install-trivy\`." >&2
    exit 1
fi

mkdir -p "$OUT_DIR"

IMAGES=$(grep -rhE '^\s*image:\s+\S' "$ROOT" 2>/dev/null \
    | awk '{print $2}' \
    | sed -E 's/^"//;s/"$//;s/^'"'"'//;s/'"'"'$//' \
    | grep -vE '\$\{' \
    | sort -u)

if [ -z "$IMAGES" ]; then
    echo "No image refs found under $ROOT/"
    exit 0
fi

total=$(printf '%s\n' "$IMAGES" | wc -l | tr -d ' ')
echo "Found ${total} unique image ref(s) under ${ROOT}/."

scanned=0
skipped=()
while IFS= read -r img; do
    safe=$(printf '%s' "$img" | tr '/:@' '___')
    out="${OUT_DIR}/sbom-k8s-${safe}.json"
    echo "scanning ${img}"
    # `trivy image --format cyclonedx` emits a CycloneDX SBOM of the image
    # contents (including vulnerability data with default scanners). `make
    # sbom-scan` later re-evaluates against the latest CVE DB and gates.
    if trivy image --quiet --format cyclonedx \
        --output "$out" "$img" 2>/dev/null; then
        scanned=$((scanned + 1))
    else
        skipped+=("$img")
        rm -f "$out"
        echo "  ::warning::sbom-kubernetes: could not scan ${img} (unreachable / auth / not found)"
    fi
done <<< "$IMAGES"

echo "Scanned ${scanned}/${total} image(s) → ${OUT_DIR}/sbom-k8s-*.json"
if [ ${#skipped[@]} -gt 0 ]; then
    printf 'Skipped %d image(s):\n' "${#skipped[@]}"
    printf '  - %s\n' "${skipped[@]}"
fi
