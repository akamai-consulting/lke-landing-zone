# llz-argo-bootstrap-apps

Chart version history. Bump `version:` in `Chart.yaml` on any template or
values change — `publish-charts.yml` pushes a chart only when its version is
not already in the registry, and never overwrites a tag, so an unbumped change
is silently never published.

## 0.1.27

Documentation only — the per-version history moved out of `Chart.yaml`
into this file. No rendered change.

## 0.1.26

re-pin llz-cluster-foundation 0.1.14 -> 0.1.15 (that chart's PSS version change, issue #447). Pin-only change here — a pin the registry never received 404s at Argo sync time, which is why the realignment ships with the bump rather than after it. Same shape as 0.1.24.

## 0.1.25

re-pin llz-cert-automation 0.1.11 -> 0.1.12 (that chart's doc-only bump for the source-ref sweep). Pin-only change, same shape as 0.1.24 and 0.1.20 — and the third time this cascade has run, which is the point of chart-pin-guard: the pin realignment is not follow-up work, it is half of the bump, because a pin the registry never received 404s at Argo sync time.

## 0.1.24

re-pin llz-cert-automation 0.1.10 -> 0.1.11 (that chart's doc-only bump). Pin-only change — a pin the registry never received 404s at Argo sync time, which is why the pin realignment ships with the bump rather than after it. Same shape as 0.1.20.

## 0.1.23

add values.schema.json — root-strict, so a typo'd top-level key is REJECTED AT RENDER instead of silently activating nothing. Nested objects stay permissive; no rendered change for valid values. Also re-pins llz-cluster-foundation 0.1.14 + llz-cert-automation 0.1.10.

## 0.1.22

doc-only README correction — drop the External Secrets Operator from the sync-order rationale (its bootstrap Application was removed in 0.1.14; apl-core 6.x ships ESO itself, as the README already said further down). Mirrors the doc-only bump of llz-cert-automation 0.1.8→0.1.9.

## 0.1.20

bump the llz-cluster-foundation pin to 0.1.12 (refactor-only NetworkPolicy template collapse; rendered output byte-identical). Pin-only change — a pin the registry never received 404s at Argo sync time.

## 0.1.18

doc-only README path refs (_shared/ → platform-apl/), and mirror the pins to the doc-only bumps of the two first-party charts it lists: llz-cluster-foundation 0.1.9→0.1.10 and llz-cert-automation 0.1.6→0.1.7.

## 0.1.17

bump the llz-cluster-foundation pin to 0.1.9 (adds harbor-allow-metrics so Prometheus can scrape Harbor's :8001 metrics endpoints, issue #183).

## 0.1.16

bump the llz-cluster-foundation pin to 0.1.8 (adds a monitoring→:8888 ingress rule so apl-core's Prometheus can scrape the OTel Collector).

## 0.1.15

bump the llz-cluster-foundation pin to 0.1.7 (that chart retired its sc-default-patcher CronJob; the PostSync Job + Kyverno policy remain).

## 0.1.14

v6 migration — remove the llz-external-secrets-operator bootstrap Application (apl-core 6.x ships ESO as a core app in `external-secrets`; a second cluster-scoped controller would fight it), retarget the AppProject destination namespace to `external-secrets`, bump the llz-cluster-foundation pin to 0.1.6, and drop the now-dead charts.external-secrets.io sourceRepos allowlist entry (the ESO App that consumed it is gone).

## 0.1.13

bump the llz-cert-automation pin to 0.1.5 (fix the unparseable harborDockerConfig configTemplate so its ExternalSecret renders).

## 0.1.12

bump the llz-cluster-foundation pin to 0.1.5 (durable sc-default-patcher CronJob backstop so a slipped-through Flux re-promotion can't leave 2 default StorageClasses wedging `llz ci converge`).

## 0.1.11

bump the llz-cert-automation pin to 0.1.4 (harbor docker-config derived in-cluster via ESO).

## 0.1.10

bump the llz-cluster-foundation pin to 0.1.4 (drops the duplicate llz-linode-volume-labeler namespace that raised a SharedResourceWarning).

## 0.1.9

doc-only — drop the stale AppRole-rotation CronWorkflow mention from the Argo Workflows comment (the subsystem was retired; no rendered change).

## 0.1.8

bump the llz-cert-automation pin to 0.1.3 (ExternalSecret refreshInterval 1h->1m).

## 0.1.7

bump the llz-cert-automation pin to 0.1.2 (JetStream version pin).

## 0.1.6

drop the eso-cert-watcher app-of-apps entry (ESO >=0.10.7 re-reads the openbao-tls CA via --store-requeue-interval; the watcher was retired).

## 0.1.4

drop the stale llz-openbao app-of-apps entry (OpenBao now ships as the apl-values/components/openbao kustomize Component; the duplicate collided).
