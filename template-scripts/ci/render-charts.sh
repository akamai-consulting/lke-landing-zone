#!/usr/bin/env bash
# render-charts.sh — Materialize the first-party Helm charts under
# kubernetes-charts/ into a flat directory of plain Kubernetes manifests.
#
# The landing-zone template ships its workload definitions AS charts (the
# apl-values manifest trees were helmified into kubernetes-charts/, see
# docs/templatization-plan.md §5). So the "instance" the kubernetes scans
# (kube-linter, kubeconform, server-side dry-run) should validate is the
# RENDERED chart output, not a raw apl-values/ tree that no longer exists here.
#
# This script is the instantiation piece: it renders every chart with its
# default values into $RENDER_DIR (one file per chart), which the scan steps in
# .github/workflows/lint.yml then point at.
#
# Usage:
#   template-scripts/ci/render-charts.sh [RENDER_DIR]
# Env:
#   RENDER_DIR   output directory (default: rendered). Also accepted as $1.
#   GIT_REPO_URL gitRepoURL passed to argo-bootstrap-apps (default: a valid
#                non-placeholder example so the chart renders without the
#                "still the placeholder" warning path).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RENDER_DIR="${1:-${RENDER_DIR:-rendered}}"
GIT_REPO_URL="${GIT_REPO_URL:-git@github.com:example/instance.git}"

cd "$ROOT"
rm -rf "$RENDER_DIR"
mkdir -p "$RENDER_DIR"

shopt -s nullglob
for dir in kubernetes-charts/*/; do
  [ -f "${dir}Chart.yaml" ] || continue
  name="$(basename "$dir")"

  # Per-chart values. The scans validate the FIRST-PARTY manifests this template
  # authors, not vendored upstream sub-charts:
  #  - argo-bootstrap-apps requires a (non-placeholder) gitRepoURL to render.
  #  - openbao-platform wraps the upstream OpenBao HA chart; disable that
  #    sub-chart (openbao.enabled=false) so we scan only our wrapper resources
  #    (TLS cert, NetworkPolicies, ServiceMonitor, rotation CronWorkflow, …).
  #    The upstream StatefulSet/Services have their own posture (writable rootfs
  #    for raft, OnDelete update strategy) and are covered by `helm lint`, not
  #    kube-linter. --namespace keeps any .Release.Namespace resource out of
  #    "default" so the use-namespace check is meaningful.
  extra=()
  ns="default"
  case "$name" in
    *argo-bootstrap-apps*) extra=(--set "global.gitRepoURL=${GIT_REPO_URL}"); ns="argocd" ;;
    *openbao-platform*)    extra=(--set "openbao.enabled=false");             ns="llz-openbao" ;;
  esac

  echo "── rendering ${name}"
  # Vendored sub-charts (openbao) live in charts/ via Chart.lock; build is a
  # no-op when already present, and must not fail the render when offline.
  helm dependency build "$dir" >/dev/null 2>&1 || true
  # --skip-tests drops Helm test hooks (e.g. *-server-test Pods): they are never
  # deployed and shouldn't be linted/dry-run as real manifests.
  helm template "$name" "$dir" --namespace "$ns" --skip-tests \
    ${extra[@]+"${extra[@]}"} >"${RENDER_DIR}/${name}.yaml"
done

echo "Rendered $(find "$RENDER_DIR" -name '*.yaml' | wc -l | tr -d ' ') chart manifest file(s) into ${RENDER_DIR}/"
