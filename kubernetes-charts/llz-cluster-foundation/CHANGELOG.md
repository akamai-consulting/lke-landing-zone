# llz-cluster-foundation

Chart version history. Bump `version:` in `Chart.yaml` on any template or
values change — `publish-charts.yml` pushes a chart only when its version is
not already in the registry, and never overwrites a tag, so an unbumped change
is silently never published.

## 0.1.16

Documentation only — the per-version history moved out of `Chart.yaml`
into this file. No rendered change.

## 0.1.15

podSecurityStandardsVersion "v1.33" -> "latest" (issue #447). RENDERED CHANGE: the enforce/warn/audit -version labels on llz-openbao and llz-observability. The pin was a whole minor behind the scaffold default and could never have been right for every instance at once — one published chart, many clusters, per-account LKE-E minors — and a lagging PSS pin silently enforces a weaker ruleset than the `restricted` label advertises.

## 0.1.14

add values.schema.json — root-strict, so a typo'd top-level key is REJECTED AT RENDER instead of silently activating nothing. Nested objects stay permissive; no rendered change for valid values.

## 0.1.12

refactor-only — collapse the four byte-identical per-namespace default-deny + allow-dns NetworkPolicy pairs into one baselineNP helper (invoked inline so document order and per-namespace grouping are kept). `helm template` output is BYTE-IDENTICAL: 21 NetworkPolicies before and after. Bumped so the guard republishes the refactored artifact.

## 0.1.10

doc-only — retarget stale apl-values/_shared/… comment path refs to the platform-apl/ layout the custom-gitops-layout branch moved to (a template comment in network-policies.yaml; behavior unchanged, bumped so the guard republishes the corrected artifact).

## 0.1.9

add harbor-allow-metrics — a monitoring→harbor:8001 ingress rule so Prometheus can scrape Harbor's metrics endpoints once metrics are enabled (issue #183). Scoped to harbor app pods on :8001; mesh is PERMISSIVE so the plaintext scrape needs no PeerAuthentication exception.

## 0.1.8

FIX otelcol_* never scraped — add a monitoring→:8888 ingress rule to observability-allow-ingress. The otel-collector-monitoring ServiceMonitor selects the target, but observability-default-deny had no rule for apl-core's Prometheus (monitoring ns), so every scrape of the OTel Collector's :8888 telemetry port timed out (context-deadline-exceeded, confirmed live) and otelcol_* was absent. Scoped to :8888, not all ingress.

## 0.1.7

retire the durable sc-default-patcher CronJob backstop (added in 0.1.5) — the sc-demote reconciler (`llz reconcile --reconcile-sc-demote`, enabled in the llz-reconciler Deployment) provides the same starvation-proof re-demote via a StorageClass watch + resync floor. The PostSync Job + Kyverno admission policy remain.

## 0.1.6

v6 migration — drop the llz-external-secrets namespace (apl-core 6.x runs ESO as a core app in `external-secrets`; the gap-fill NPs now ship from the externalSecrets apl-values component) and refresh the apl-network-policies coverage note for 6.x; also document in coredns-custom.yaml that the *.internal rewrite stays required on 6.x (the oauth2-proxy init container still resolves keycloak.<domainSuffix>) and that narrowing to a per-host allowlist is lab-gated — comment-only, no rendered resource change.

## 0.1.5

add a durable sc-default-patcher CronJob backstop (every 2m) so a Flux re-promotion of linode-block-storage-retain that slips past the Kyverno admission policy is re-demoted even after Flux goes quiet — the PostSync Job + admission-only policy could leave 2 defaults wedging `llz ci converge`.

## 0.1.4

drop the llz-linode-volume-labeler namespace from .namespaces — the volume-labeler tree in apl-values (platform-bootstrap) is its sole owner now, so declaring it here too raised a SharedResourceWarning that pinned platform-bootstrap OutOfSync.

## 0.1.3

add cert-manager-allow-webhook-linode-ingress NP so the apiserver can reach the Linode DNS-01 webhook's :443 (else acme.slicen.me APIService stays Available=False and convergence hangs).

## 0.1.2

strip internal fingerprints from comments for open-sourcing (no rendered change).
