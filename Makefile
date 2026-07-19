SHELL := /bin/bash

.PHONY: help \
        build build-tools llz \
        fmt fmt-check vet shellcheck audit update tidy sbom gitleaks \
        sbom-go sbom-terraform sbom-kubernetes sbom-scan \
        chart-pin-guard chart-version-guard \
		tf-fmt tf-fmt-check tf-lint tf-validate tf-validate-roots checkov workflows-lock-check render-charts k8s-lint k8s-validate chart-guards prom-rules-check helm-repos helm-lint-real-values helm-lint-charts helm-dep-lock-check argocd-rendered-apps-check externalsecret-paths-check wave-health-guard wave-dependency-guard mesh-egress-guard monitoring-label-guard untestable-loc-check actions-lint placeholder-guard template-manifest-check lint lint-k8s lint-tf \
        test coverage clean \
        instance-test scaffold-check llz-functional reap-orphans \
        install-tools install-syft install-trivy install-gitleaks

KUBECTL_VERSION  := 1.31.0

# The Go module that holds the host-side tooling: tools/ (the `llz` CLI). The
# firewall-cidrs / firewall-controller commands moved to the private
# lke-landing-zone-internal repo.
GO_DIR := tools
# Bounded retry wrapper for flaky network fetches (helm index refreshes / chart
# pulls) — a transient upstream 5xx/DNS blip shouldn't fail a build. See the script.
RETRY := template-scripts/ci/with-retry.sh

# Per-package minimum statement coverage, as <pkg-suffix>=<percent> entries.
# pkg-suffix matches the END of a Go import path (cmd/llz -> .../tools/cmd/llz).
# `make coverage` fails the build if any listed package drops below its floor.
# It's a ratchet: bump a floor UP as that package's coverage improves, never
# down.
# Override on the CLI, e.g. `make coverage COVERAGE_MINS="cmd/llz=20"`.
COVERAGE_MINS := \
	cmd/llz=48 \
	internal/cli=95 \
	internal/clusterspec=95 \
	internal/health=95 \
	internal/kube=78 \
	internal/linode=80 \
	internal/metrics=95 \
	internal/openbao=88 \
	internal/preflight=95 \
	internal/terraform=95

help:
	@echo "lke-landing-zone — template repository targets"
	@echo
	@echo "Go targets:"
	@echo "  build           Build the Go tools (+ llz)"
	@echo "  build-tools     go build ./... in tools/"
	@echo "  llz             Build the adopter CLI to bin/llz"
	@echo "  fmt             gofmt -w (auto-fix formatting)"
	@echo "  fmt-check       gofmt -l (CI-safe, no writes)"
	@echo "  vet             go vet ./... across the tools module"
	@echo "  shellcheck      shellcheck every *.sh in the repo (+ template-scripts/hooks)"
	@echo "  audit           govulncheck ./... — Go vulnerability database scan"
	@echo "  tidy            go mod tidy (and verify it leaves no diff)"
	@echo "  update          go get -u ./... + go mod tidy (bump dependencies)"
	@echo "  gitleaks        gitleaks secret scan of the working tree"
	@echo "  sbom            Generate CycloneDX SBOMs into sbom/"
	@echo "  test            go test ./... in tools/"
	@echo "  coverage        go test -cover for the tools module (fails below per-pkg COVERAGE_MINS)"
	@echo "  clean           Remove build + coverage artifacts"
	@echo
	@echo "Terraform targets:"
	@echo "  tf-fmt          tofu fmt (auto-fix formatting)"
	@echo "  tf-fmt-check    tofu fmt -check (CI-safe, no writes)"
	@echo "  tf-lint         tflint — Terraform best-practice rules (.tflintrc.hcl)"
	@echo "  tf-validate     terraform validate — syntax + type checking (inits each module first)"
	@echo "  checkov         Checkov IaC security scan across all Terraform modules"
	@echo
	@echo "Kubernetes targets:"
	@echo "  k8s-lint        kube-linter — k8s best-practice checks (.kube-linter.yaml)"
	@echo "  k8s-validate    kubeconform — schema validation against k8s 1.31"
	@echo "  prom-rules-check  promtool check rules — PromQL syntax + rule structure"
	@echo "  helm-lint-charts  helm lint --strict + template every first-party chart"
	@echo "  helm-lint-real-values  hard dep-build + namespaced render of the OpenBao chart (lint --strict is helm-lint-charts' job)"
	@echo "  helm-dep-lock-check  verify committed Chart.lock files match Chart.yaml dependency declarations"
	@echo "  chart-guards    run BOTH chart guards (version bump + Argo pin realignment) — a bump needs both"
	@echo "  argocd-rendered-apps-check  render overlays and reject duplicate ArgoCD Helm parameters"
	@echo "  externalsecret-paths-check  validate ExternalSecret refs and OpenBao policy coverage"
	@echo "  wave-health-guard           negative-sync-wave kinds must be health-safe (PR #142 wedge class)"
	@echo "  wave-dependency-guard       a workload must sync AFTER the ExternalSecret it hard-depends on (#163 wedge class)"
	@echo "  mesh-egress-guard           no NetworkPolicy egress to a STRICT-mesh namespace (harbor) from outside it"
	@echo "  monitoring-label-guard      every ServiceMonitor/PodMonitor/PrometheusRule carries prometheus: system (#175 day-2-blind class)"
	@echo "  untestable-loc-check  fail when inline-bash/shell/python logic exceeds .untestable-budget.yaml"
	@echo "  actions-lint    actionlint — GitHub Actions workflow linting"
	@echo "  lint            Changed-file linters; LINT_ALL=1 runs the full local mirror of"
	@echo "                  the CI 'Lint' workflow (.github/workflows/lint.yml): go + shell +"
	@echo "                  py + actions, \$$(LINT_TF), and \$$(LINT_K8S). The kind server-side"
	@echo "                  dry-run is CI-only (needs Docker/kind)."
	@echo
	@echo "Instance test:"
	@echo "  instance-test   Local, no-cloud smoke test: copier-instantiate the template"
	@echo "                  and run the offline validators (token residue, structure,"
	@echo "                  terraform validate, actionlint) against the rendered instance."
	@echo "                  The fast counterpart to release-e2e (which stands up a real"
	@echo "                  cluster); also the CI 'instantiate' job. Runs scaffold-check"
	@echo "                  first. Self-skips without copier; SKIP_TF=1 skips tf validate."
	@echo "  scaffold-check  Scaffold a throwaway env (llz env add) and assert the"
	@echo "                  per-env scaffold renders: no leftover 'your-env', required"
	@echo "                  per-env files present, values.yaml renders via templatefile()"
	@echo "                  (vars derived from main.tf), and passes apl-core's helm schema."
	@echo "                  No cloud; artifacts removed on exit. SKIP_TF=1 skips render;"
	@echo "                  the schema step self-skips without helm."
	@echo "  reap-orphans    Manual sweep of leaked Linode resources from failed/cancelled"
	@echo "                  cycles: orphan clusters (if CLUSTER_LABEL) + their firewall/VPC,"
	@echo "                  then NodeBalancers + VPCs + Volumes whose cluster is gone."
	@echo "                  DRY-RUN unless CONFIRM=yes. Needs LINODE_TOKEN; REGION recommended."
	@echo
	@echo "Setup:"
	@echo "  install-tools   Install all required Go and system tools for local development"

# ── Tools ────────────────────────────────────────────────────────────────────

install-tools: install-syft install-trivy
	go install golang.org/x/vuln/cmd/govulncheck@latest
	@if command -v brew >/dev/null 2>&1; then \
		brew install actionlint checkov helm; \
	else \
		pip3 install --user checkov; \
		curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash; \
		curl -fsSL https://raw.githubusercontent.com/rhysd/actionlint/main/scripts/download-actionlint.bash | bash; \
	fi

# Pinned, SHA-verified syft install. Used ONLY by sbom-terraform — trivy does
# not parse .terraform.lock.hcl for provider inventory; everything else uses
# trivy. Override SYFT_VERSION / SYFT_INSTALL_DIR via env.
install-syft:
	@./template-scripts/ci/install-syft.sh

# Pinned, SHA-verified trivy install. Used by sbom-kubernetes and sbom-scan.
# Override TRIVY_VERSION / TRIVY_INSTALL_DIR via env.
install-trivy:
	@./template-scripts/ci/install-trivy.sh

# Pinned, SHA-verified gitleaks install. Used by the `gitleaks` secret-scan gate.
# Override GITLEAKS_VERSION / GITLEAKS_INSTALL_DIR via env.
install-gitleaks:
	@./template-scripts/ci/install-gitleaks.sh

# ── Build ────────────────────────────────────────────────────────────────────

build: build-tools llz

build-tools:
	cd $(GO_DIR) && go build ./...

# Local dev build of the adopter CLI; release builds are multi-platform (see
# .github/workflows/llz-release.yml). Adopters install the released binary.
llz:
	cd $(GO_DIR) && go build -o ../bin/llz ./cmd/llz

# ── Lint ─────────────────────────────────────────────────────────────────────

fmt:
	cd $(GO_DIR) && gofmt -w .

fmt-check:
	@cd $(GO_DIR) && out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files need formatting:"; echo "$$out"; exit 1; \
	fi

vet:
	cd $(GO_DIR) && go vet ./...

# Every shell script in the repo, not just template-scripts/: .github/scripts/
# dispatch-and-watch.sh and instance-template/.github/actions/_lib/git-auth.sh were
# never linted, yet `make lint`'s router fires shellcheck on ANY *.sh change — so
# editing either one triggered a check that then skipped it.
shellcheck:
	find . -path ./.git -prune -o \( -name '*.sh' -o -path './template-scripts/hooks/*' \) -type f -print0 \
		| xargs -0 shellcheck -x

# ── Terraform ─────────────────────────────────────────────────────────────────

# The template's first-party Terraform surface is the reusable modules. The
# per-env instance roots live under instance-template/terraform-iac-bootstrap/ and are
# scaffolding (placeholders + git:: tags that resolve only after publishing), so
# they are not linted here.
TF_DIRS := $(wildcard terraform-modules/llz-cluster \
                      terraform-modules/llz-pool \
                      terraform-modules/llz-object-storage \
                      terraform-modules/llz-node-firewall)

tf-fmt:
	@for d in $(TF_DIRS); do tofu fmt "$$d"; done

tf-fmt-check:
	@for d in $(TF_DIRS); do tofu fmt -check "$$d" || exit 1; done

tf-lint:
	@for d in $(TF_DIRS); do tflint --chdir="$$d" --config="$(CURDIR)/.tflintrc.hcl" || exit 1; done

# tf-validate: HCL syntax + provider type checking. Runs its own
# `init -backend=false` per module first — it previously documented prior init as
# a precondition, but no target performs one for TF_DIRS, so the target could
# never succeed as written. Both working counterparts (tf-validate-roots and
# llz's stepTFValidate) already init themselves; this matches them.
tf-validate:
	@for d in $(TF_DIRS); do \
		terraform -chdir="$$d" init -backend=false -input=false >/dev/null || exit 1; \
		terraform -chdir="$$d" validate || exit 1; \
	done

checkov:
	@for d in $(TF_DIRS); do checkov -d "$$d" --framework terraform --config-file .checkov.yaml --compact --quiet || exit 1; done

# tf-validate-roots: validate the INSTANCE Terraform roots (the reusable modules
# are covered by tf-fmt-check/tf-lint/tf-validate/checkov above). Instantiates
# the roots by rewriting their published git:: module sources to the in-repo
# terraform-modules/ paths, then runs init -backend=false + validate on each —
# catching HCL/type/module-wiring errors without published tags or remote state.
tf-validate-roots:
	template-scripts/ci/instantiate-terraform.sh

# ── Kubernetes ────────────────────────────────────────────────────────────────

# RENDER_DIR: where template-scripts/ci/render-charts.sh materializes the first-party
# charts as plain Kubernetes manifests. The landing-zone template ships its
# workloads AS charts (the apl-values manifest trees were helmified into
# kubernetes-charts/, templatization §5), so the kubernetes scans validate this
# rendered output rather than a raw apl-values/ tree that no longer exists here.
RENDER_DIR ?= rendered

render-charts:
	template-scripts/ci/render-charts.sh $(RENDER_DIR)

# k8s-lint: kube-linter checks the rendered first-party charts against the
# all-built-in ruleset (security contexts, anti-affinity, resource limits, …).
# Policy + exclusions live in .kube-linter.yaml.
k8s-lint: render-charts
	kube-linter lint $(RENDER_DIR) --config .kube-linter.yaml

# k8s-validate: kubeconform schema validation of the rendered charts against the
# Kubernetes schema + the Datree CRD catalog. -ignore-missing-schemas tolerates
# CRs whose CRDs aren't in the catalog — those are validated against the real
# installed CRDs by the kind dry-run job in .github/workflows/lint.yml.
#   ClusterIssuer — Datree catalog rejects cert-manager's dns01.selector field
K8S_VALIDATE_SKIP_KINDS := ClusterIssuer
k8s-validate: render-charts
	kubeconform \
	  -kubernetes-version $(KUBECTL_VERSION) \
	  -ignore-missing-schemas \
	  -schema-location default \
	  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
	  -skip $(K8S_VALIDATE_SKIP_KINDS) \
	  -summary \
	  -output pretty \
	  $(RENDER_DIR)

# Validate every Prometheus rule file under apl-values shipped as PrometheusRule
# CRs. Apl-core's kube-prometheus-stack picks them up via its ruleSelector
# matching the labels on each CRD. promtool only accepts the bare-groups form,
# so `llz ci check-prom-rules` extracts spec.groups from each CRD before invoking
# it. The rules live in the observability component's prometheus-rules/ tree
# (openbao-alerts, support-plane-alerts, …) — the previous default pointed at
# the retired prometheus-rules-crd path, so the gate skip-cleaned on every run
# and nothing promtool-validated the live rules. `llz ci check-prom-rules` is
# the native port of the former template-scripts/linting-and-validation/
# check-prometheus-rule-crds.py; uses the PATH llz when present (the CI images
# bake it), else builds from source.
# LLZ_CI — invoke one `llz ci <verb>`. Nine targets stamped out this same
# if/else, and getting it wrong is not cosmetic: the two branches had already
# drifted (most pass --root, prom-rules passes --rules-dir instead) and, worse,
# the PATH branch silently wins on a workstation where `llz` is whatever you
# last installed — so a guard can report a pass that says nothing about your
# working tree.
#
#   $(1) verb + the flags that read the same from either branch
#   $(2) flags needed ONLY when running from $(GO_DIR) (re-basing relative paths)
#
# PATH-first is right for CI: the ci-kubernetes / ci-terraform images bake llz
# and carry no Go toolchain, so `go run` cannot fire there at all. Set
# LLZ_FORCE_SOURCE=1 to invert it and always build from source — what you want
# locally the moment you have touched tools/. `make chart-guards` sets it.
# NOTE ON THE ASYMMETRY: $(2) is passed ONLY to the source branch, and that is
# deliberate — do not "fix" it by threading $(2) into the PATH branch. The two
# branches run from different working directories: the source branch cds into
# $(GO_DIR), so it needs `--root ..` to climb back to the repo root, while the
# PATH branch already runs there and its `--root .` default is correct. Passing
# `--root ..` to the PATH branch would point every guard OUTSIDE the repo.
#
# NOTE ON VERSION SKEW: the PATH branch runs whatever `llz` is installed, which
# is NOT your working tree. That is the point in CI (the images bake a known
# build), but locally it means editing a guard and running its make target can
# report the OLD guard's verdict — a silent, very convincing wrong answer. So
# the branch taken is announced on every run. Set LLZ_FORCE_SOURCE=1 to always
# build from source when iterating on a guard.
define LLZ_CI
	@if [ -z "$$LLZ_FORCE_SOURCE" ] && command -v llz >/dev/null 2>&1; then \
		echo "[llz: $$(command -v llz) $$(llz version 2>/dev/null | head -1) — NOT your working tree; LLZ_FORCE_SOURCE=1 to build from source]"; \
		llz ci $(1); \
	else \
		echo "[llz: built from source]"; \
		cd $(GO_DIR) && go run ./cmd/llz ci $(1) $(2); \
	fi
endef

prom-rules-check:
	$(call LLZ_CI,check-prom-rules,--rules-dir ../platform-apl/components/observability/prometheus-rules)

helm-repos:
	$(RETRY) helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
	$(RETRY) helm repo add grafana https://grafana.github.io/helm-charts --force-update
	$(RETRY) helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts --force-update
	$(RETRY) helm repo add harbor https://helm.goharbor.io --force-update
	$(RETRY) helm repo add openbao          https://openbao.github.io/openbao-helm                  --force-update
	$(RETRY) helm repo add argo             https://argoproj.github.io/argo-helm                   --force-update
	$(RETRY) helm repo add jetstack         https://charts.jetstack.io                             --force-update
	$(RETRY) helm repo add external-secrets https://charts.external-secrets.io                    --force-update
	$(RETRY) helm repo update

# The OpenBao chart was extracted/decoupled into kubernetes-charts/llz-openbao-platform (the
# first-party chart library, published to GHCR). These targets keep the dedicated
# CronWorkflow/dep-lock validation pointed at it; helm-lint-charts also lints it
# along with every other first-party chart. The chart's defaults live in its
# values.yaml (the former openbao-values.yaml content was merged in on extract).
# cert-manager, ESO, kube-prometheus-stack, Grafana, Loki, OTel, and Harbor are
# installed by apl-core directly — validated by `make argocd-rendered-apps-check`.
OPENBAO_CHART := kubernetes-charts/llz-openbao-platform

# helm-lint-real-values: the two OpenBao checks helm-lint-charts does NOT cover.
# `helm dependency build` runs HARD here — helm-lint-charts soft-ignores build
# failures (`|| true`) so one chart's broken dependency cannot mask lint for the
# rest, which means nothing else fails the build on it. The render is namespaced
# and uses the real release name, exercising the templates that key off
# .Release.Namespace/.Release.Name; helm-lint-charts renders with defaults only.
#
# `helm lint --strict` deliberately is NOT repeated here: helm-lint-charts
# already lints every first-party chart including this one, and this target and
# the now-deleted helm-lint-argocd each used to repeat it, linting one chart
# three times per lint-k8s run.
helm-lint-real-values: helm-repos
	helm dependency build $(OPENBAO_CHART)
	helm template platform-openbao $(OPENBAO_CHART) \
		-n llz-openbao >/dev/null

argocd-rendered-apps-check: render-charts
	$(call LLZ_CI,argocd-rendered-apps --render-dir $(RENDER_DIR),--root ..)

# placeholder-guard: reject unsubstituted placeholder.example.com hostnames in the
# rendered manifests. Was nine lines of inline recipe bash hand-rolling the
# empty-corpus assertion and the tree walk the guard framework already owns
# (requireCorpus + walkManifests) — the last un-unit-tested guard in LINT_K8S.
placeholder-guard: render-charts
	$(call LLZ_CI,placeholder-guard --render-dir $(RENDER_DIR),--root ..)

# externalsecret-paths-check: `llz ci externalsecret-paths` (the native port of
# the former validate-externalsecret-paths.py). Uses the PATH llz when present
# (the CI images bake it); otherwise builds from source via the Go toolchain.
externalsecret-paths-check: export RENDER_DIR := $(RENDER_DIR)
externalsecret-paths-check: render-charts
	$(call LLZ_CI,externalsecret-paths,--root ..)

# wave-health-guard: `llz ci wave-health-guard` — the PR #142 wedge-class gate.
# Argo sync waves gate on per-resource health; a health-checked kind at a
# negative wave that can be not-Ready on a fresh cluster wedges the
# platform-bootstrap sync before OpenBao (wave 0). Every negative-wave kind in
# platform-apl/manifest/ + platform-apl/components/ must be health-inert or
# backed by a resource.customizations.health override in apl-values/values.yaml.
wave-health-guard:
	$(call LLZ_CI,wave-health-guard,--root ..)

# wave-dependency-guard: `llz ci wave-dependency-guard` — the #163 wedge-class gate.
# Argo sync waves gate on per-resource health, so a Deployment/StatefulSet/DaemonSet
# that hard-references a Secret produced by a LATER-wave ExternalSecret can never go
# Healthy — it wedges the platform-bootstrap sync and starves every later-wave
# ExternalSecret in it (in #163 the wave-0 reconciler Deployment took harbor +
# loki's wave-5 object-store secrets down). A workload's wave must exceed the wave
# of every ExternalSecret whose Secret it hard-depends on (optional refs exempt).
wave-dependency-guard:
	$(call LLZ_CI,wave-dependency-guard,--root ..)

# mesh-egress-guard: `llz ci mesh-egress-guard` — the harbor-reconciler mesh class.
# apl-core runs platform namespaces (harbor) under Istio STRICT mTLS; a pod OUTSIDE
# that mesh can't reach a Service inside it (dropped at the sidecar, not by
# NetworkPolicy). Flags any NetworkPolicy egress to a STRICT-mesh namespace from a
# different namespace — the batch-5 harbor reconciler's mistake, caught at PR time
# instead of two ~50-minute e2e cycles.
mesh-egress-guard:
	$(call LLZ_CI,mesh-egress-guard,--root ..)

# monitoring-label-guard: `llz ci monitoring-label-guard` — the #175 day-2-blind
# class. apl-core's Prometheus selects ServiceMonitors / PodMonitors /
# PrometheusRules by {prometheus: system}; a CR without the label is silently
# ignored (metrics unscraped / rules unloaded / alerts never firing) — a class
# promtool and kube-linter both pass. #175 was 5 CRs missing the label, blinding
# the whole day-2 signal, undetectable except on a live cluster. Scans the
# rendered chart output too (the openbao ServiceMonitor is a chart template), so
# it depends on render-charts.
monitoring-label-guard: render-charts
	$(call LLZ_CI,monitoring-label-guard,--root ..)

# untestable-loc-check: the design-principle gate. Fails when inline workflow
# bash / shell / python logic exceeds the budget in .untestable-budget.yaml —
# the signal to convert logic into the unit-tested llz CLI rather than pile more
# untestable shell into CI. Pure Go + a config file, so it runs anywhere (no
# rendered charts needed). Budgets ratchet DOWN as code is converted.
untestable-loc-check:
	$(call LLZ_CI,untestable-loc,--root ..)

# chart-guards: the two halves of "I changed a chart" — run them together.
# Bumping a Chart.yaml version is only half the job: the bump leaves every Argo
# pin on the OLD version, and chart-version-guard passing says nothing about
# whether chart-pin-guard does. Realigning a pin can itself change another
# chart's values.yaml and require a second bump, so this may take two passes —
# re-run until clean. CI runs both in the same job ("Charts bump version and
# pins stay aligned"); this is the local equivalent.
#
# Runs both from SOURCE via LLZ_FORCE_SOURCE. The default PATH-first resolution
# in LLZ_CI is right for CI but wrong here: on a workstation `llz` is whatever
# binary you last installed, so the guards would run months-old code against
# today's working tree and report a pass that means nothing. That is the exact
# failure this target exists to prevent, so it opts out. CI is unaffected — it
# calls `llz ci ...` directly with a binary built in the same job.
chart-guards: export LLZ_FORCE_SOURCE := 1
chart-guards: chart-version-guard chart-pin-guard

# cosign-subject-guard: assert every Kyverno keyless `subject:` that names a
# GitHub Actions workflow still resolves to a workflow that exists. Keyless
# signing derives the cert subject from the workflow PATH, so renaming the
# signing workflow silently invalidates every signature the policy accepts —
# and that surfaces as pods failing admission in downstream clusters, not here.
cosign-subject-guard:
	cd $(GO_DIR) && go run ./cmd/llz ci cosign-subject-guard --root ..

# chart-pin-guard: assert every Argo CD first-party chart pin (apl-values
# targetRevision + llz-argo-bootstrap-apps component version) matches the chart's
# local kubernetes-charts/<chart>/Chart.yaml version. A pin the registry never
# received 404s at Argo sync time — on a cold bootstrap that silently strands the
# support-plane app (llz-openbao namespace never created) and times out the
# OpenBao bootstrap. Decision logic is unit-tested Go; this is thin glue.
chart-pin-guard:
	$(call LLZ_CI,chart-pin-guard,--root ..)

# chart-version-guard: assert every chart whose directory changed vs the base ref
# bumped its Chart.yaml `version:`. publish-charts.yml publishes immutably (it only
# pushes a NEW version), so a template/values change merged WITHOUT a bump is
# silently never released and clusters keep pulling the stale artifact. CI runs this
# in its own workflow with the PR base SHA; the local mirror diffs against
# origin/main (override with CHART_GUARD_BASE=<ref>). Kept OUT of LINT_K8S on
# purpose — the CI lint-k8s container has no base ref to diff against. Decision
# logic is unit-tested Go; this is thin glue.
CHART_GUARD_BASE ?= origin/main
chart-version-guard:
	$(call LLZ_CI,chart-version-guard --base $(CHART_GUARD_BASE),--root ..)

helm-dep-lock-check:
	cd $(GO_DIR) && go run ./cmd/llz ci chart-lock-drift --root .. $(OPENBAO_CHART)

# helm-lint-charts: lint + template every first-party Helm chart under kubernetes-charts/.
# These are the extracted, independently-versioned charts published to GHCR
# (docs/templatization-plan.md §5). `helm lint --strict` enforces schema +
# best-practices; `helm template` proves every chart renders with its default
# values (the operational scars are encoded as those defaults). Mirrors the
# helm-lint-charts CI step in .github/workflows/lint.yml.
helm-lint-charts: helm-repos
	@set -euo pipefail; \
	for dir in kubernetes-charts/*/; do \
		[ -f "$${dir}Chart.yaml" ] || continue; \
		echo "── $$dir"; \
		helm dependency build "$$dir" >/dev/null 2>&1 || true; \
		helm lint --strict "$$dir"; \
		helm template "$$(basename "$$dir")" "$$dir" >/dev/null; \
	done

actions-lint:
	actionlint .github/workflows/*.yml

# (sync-wave-lint lived here. It grepped whole FILES for `^kind: (Application|
# AppProject)` and then for the sync-wave string anywhere in that same file, so
# one annotated Application satisfied the check for every other Application Helm
# rendered beside it. It also matched the annotation name in a comment, never
# checked the value parsed as an integer, and passed vacuously when RENDER_DIR
# was empty (the find loop simply never ran). Folded into `llz ci
# argocd-rendered-apps`, which already decodes every document individually over
# the same corpus and already fails loudly on an empty render dir.)

# ── Combined lint ─────────────────────────────────────────────────────────────
# By default, only lints files changed since the last commit (git diff HEAD).
# Pass LINT_ALL=1 to run every check unconditionally (e.g. in CI).

# Kubernetes + Terraform check groups — the single source of truth shared by the
# CI 'Lint' workflow (.github/workflows/lint.yml, via the lint-k8s / lint-tf
# entrypoints below) and the local `make lint` mirror. The render-based k8s
# targets share a render-charts prerequisite, so one $(MAKE) invocation renders
# once. tf-fmt-check is kept OUT of LINT_TF (it uses tofu, absent from the CI
# TF_IMAGE) and added explicitly to the local all-checks run.
LINT_K8S := k8s-lint k8s-validate wave-health-guard wave-dependency-guard mesh-egress-guard monitoring-label-guard placeholder-guard \
            externalsecret-paths-check argocd-rendered-apps-check chart-pin-guard prom-rules-check \
            cosign-subject-guard \
            helm-lint-charts helm-lint-real-values \
            helm-dep-lock-check
LINT_TF := tf-lint checkov tf-validate-roots

# CI job entrypoints — one target per lint.yml container job.
lint-k8s: $(LINT_K8S) shellcheck
lint-tf: $(LINT_TF) template-manifest-check workflows-lock-check

# Assert .template-manifest classifies every scaffold file (managed/merge/owned),
# so the template-update tooling never has to guess about a new file.
# Both branches need a --root, and they need DIFFERENT ones (the source branch
# runs from $(GO_DIR), one level down). $(1) carries the repo-root spelling and
# $(2) appends the re-based one, so the source branch passes --root twice and the
# LAST occurrence wins — pflag's documented behaviour, verified. Don't "fix" the
# apparent duplicate: dropping either one breaks one of the two branches.
template-manifest-check:
	$(call LLZ_CI,template-manifest --root instance-template,--root ../instance-template)

# Assert instance-template/.template-workflows.lock still matches the vendored
# .github/ files it covers. Editing a llz-*.yml body without re-running
# `llz ci workflows-fresh --write` would ship a lock that every instance fails on,
# so catch it here instead. Same two-branch --root trick as above (last wins).
workflows-lock-check:
	$(call LLZ_CI,workflows-fresh --root instance-template,--root ../instance-template)

lint:
	@set -e; \
	if [ -n "$(LINT_ALL)" ]; then \
		$(MAKE) --no-print-directory fmt-check vet shellcheck actions-lint tf-fmt-check template-manifest-check workflows-lock-check untestable-loc-check $(LINT_TF) $(LINT_K8S) chart-version-guard instance-test; \
		LLZ_FUNCTIONAL_NET=0 $(MAKE) --no-print-directory llz-functional; \
		exit 0; \
	fi; \
	CHANGED=$$(git diff --name-only HEAD 2>/dev/null || git ls-files); \
	if [ -z "$$CHANGED" ]; then \
		echo "lint: nothing changed since last commit (use LINT_ALL=1 to run all checks)"; \
		exit 0; \
	fi; \
	if echo "$$CHANGED" | grep -qE '\.go$$|go\.(mod|sum)$$'; then \
		$(MAKE) --no-print-directory fmt-check vet; \
	fi; \
	if echo "$$CHANGED" | grep -qE '^tools/.*\.go$$|^tools/go\.(mod|sum)$$|^template-scripts/ci/llz-functional\.sh$$'; then \
		LLZ_FUNCTIONAL_NET=0 $(MAKE) --no-print-directory llz-functional; \
	fi; \
	if echo "$$CHANGED" | grep -qE '\.sh$$|template-scripts/hooks/'; then \
		$(MAKE) --no-print-directory shellcheck; \
	fi; \
	if echo "$$CHANGED" | grep -qE '^(terraform-modules|instance-template/terraform-iac-bootstrap)/.*\.tf$$|\.tflintrc\.hcl$$|\.checkov\.yaml$$'; then \
		$(MAKE) --no-print-directory tf-fmt-check $(LINT_TF); \
	fi; \
	if echo "$$CHANGED" | grep -qE '^instance-template/apl-values/|^platform-apl/|^tools/cmd/llz/ci_bootstrap_cluster\.go$$|^template-scripts/ci/scaffold-render-check\.sh$$'; then \
		$(MAKE) --no-print-directory wave-health-guard scaffold-check; \
	fi; \
	if echo "$$CHANGED" | grep -qE '^copier\.yml$$|^instance-template/\.github/|^template-scripts/ci/instance-test\.sh$$'; then \
		$(MAKE) --no-print-directory instance-test; \
	fi; \
	if echo "$$CHANGED" | grep -qE '^kubernetes-charts/|\.kube-linter\.yaml$$'; then \
		$(MAKE) --no-print-directory $(LINT_K8S); \
	fi; \
	if echo "$$CHANGED" | grep -qE '^kubernetes-charts/'; then \
		$(MAKE) --no-print-directory chart-version-guard; \
	fi; \
	if echo "$$CHANGED" | grep -qE '\.github/workflows/.*\.yml$$'; then \
		$(MAKE) --no-print-directory actions-lint; \
	fi; \
	if echo "$$CHANGED" | grep -qE '\.github/workflows/.*\.yml$$|\.sh$$|instance-template/\.github/|^\.untestable-budget\.yaml$$'; then \
		$(MAKE) --no-print-directory untestable-loc-check; \
	fi

# ── Audit ─────────────────────────────────────────────────────────────────────

# govulncheck cross-references the Go vulnerability database against the symbols
# the tools module actually calls. `go run ...@latest` avoids a separate install
# step; pin GOVULNCHECK_VERSION to a tag for reproducible CI if desired.
GOVULNCHECK_VERSION ?= latest

audit:
	cd $(GO_DIR) && go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# tidy verifies go.mod / go.sum are in sync with the source (CI-safe: fails if
# `go mod tidy` would change anything).
tidy:
	@cd $(GO_DIR) && go mod tidy && \
	if ! git diff --quiet -- go.mod go.sum; then \
		echo "go.mod / go.sum are not tidy — run 'make tidy' and commit the result"; \
		git --no-pager diff -- go.mod go.sum; exit 1; \
	fi

update:
	cd $(GO_DIR) && go get -u ./... && go mod tidy

# Secret scan of the full git history (honours .gitleaks.toml allowlists).
# Auto-installs a pinned, SHA-verified gitleaks (into $HOME/.local/bin) when the
# binary is absent — same self-bootstrapping convention as the actionlint target
# — so the CI gate and a fresh checkout both Just Work. --redact keeps any match
# out of the logs; the non-zero exit on a finding is what makes this a gate.
gitleaks:
	@command -v gitleaks >/dev/null 2>&1 || ./template-scripts/ci/install-gitleaks.sh
	@PATH="$$HOME/.local/bin:$$PATH" gitleaks detect --source . --redact --no-banner

# SBOM generation — release evidence. Three sources:
#   * sbom-go         — `trivy fs` CycloneDX SBOM of the Go tools module.
#   * sbom-terraform  — `syft scan` against terraform-iac-bootstrap/ (parses
#                       every .terraform.lock.hcl for provider versions).
#                       Trivy doesn't parse Terraform lock files; syft does,
#                       so syft is retained here even though trivy owns the
#                       rest of the SBOM + CVE pipeline.
#   * sbom-kubernetes — `trivy image` per container image referenced under
#                       kubernetes/ (template-scripts/ci/sbom-kubernetes.sh extracts the
#                       refs and runs trivy per ref).
# All three produce CycloneDX JSON in sbom/ so the release.yml SBOM job can
# upload them with a single `gh release upload sbom/*.json`.
#
# `make sbom-scan` runs trivy against the produced SBOMs and exits non-zero
# on Critical CVEs — release.yml runs this after sbom to gate the release.
SYFT ?= syft
TRIVY ?= trivy
SBOM_FAIL_ON ?= CRITICAL

sbom: sbom-go sbom-terraform sbom-kubernetes

# SBOM_STEP — one sbom-* step: run $(2) if the tool is on PATH, else warn and
# skip so a local `make sbom` still produces what it can. CI installs both tools
# first (release.yml); SBOM_STRICT=1 turns the skip into a failure.
#
# Three targets stamped out this same block, differing only in tool, command and
# label — the shape LLZ_CI above was introduced to kill, with the same drift risk
# already realised once (sbom-scan below had diverged).
#
#   $(1) tool binary   $(2) command to run   $(3) target name (for the message)
define SBOM_STEP
	@mkdir -p sbom
	@if command -v $(1) >/dev/null 2>&1; then \
		$(2); \
	else \
		echo "WARNING: $(1) not installed — skipping $(3)."; \
		echo "  Install: make install-$(1)"; \
		[ -z "$(SBOM_STRICT)" ] || { echo "SBOM_STRICT=1 set — failing"; exit 1; }; \
	fi
endef

sbom-go:
	$(call SBOM_STEP,$(TRIVY),$(TRIVY) fs --quiet --format cyclonedx --output sbom/sbom-tools.json $(GO_DIR),sbom-go)

sbom-terraform:
	$(call SBOM_STEP,$(SYFT),$(SYFT) scan dir:terraform-iac-bootstrap -o cyclonedx-json=sbom/sbom-terraform.json,sbom-terraform)

sbom-kubernetes:
	$(call SBOM_STEP,$(TRIVY),./template-scripts/ci/sbom-kubernetes.sh,sbom-kubernetes)

# Vulnerability gate. Reads the generated SBOMs in sbom/ and fails on any CVE
# at or above SBOM_FAIL_ON severity (default CRITICAL). Override via env to
# tighten (CRITICAL,HIGH) or loosen (UNKNOWN to disable the gate). Skipped
# locally if trivy is missing unless SBOM_STRICT=1.
sbom-scan:
	@if ! command -v $(TRIVY) >/dev/null 2>&1; then \
		echo "WARNING: trivy not installed — skipping sbom-scan."; \
		[ -z "$(SBOM_STRICT)" ] || { echo "SBOM_STRICT=1 set — failing"; exit 1; }; \
		exit 0; \
	fi
	@if [ -z "$$(ls sbom/*.json 2>/dev/null)" ]; then \
		echo "No SBOMs in sbom/ — run \`make sbom\` first."; \
		exit 1; \
	fi
	@fail=0; \
	for f in sbom/*.json; do \
		echo "Scanning $$f for $(SBOM_FAIL_ON)+ vulnerabilities..."; \
		$(TRIVY) sbom --quiet --severity $(SBOM_FAIL_ON) \
			--exit-code 1 --no-progress "$$f" || fail=1; \
	done; \
	if [ "$$fail" -ne 0 ]; then \
		echo "::error::sbom-scan: $(SBOM_FAIL_ON)+ vulnerabilities present (see output above)"; \
		exit 1; \
	fi
	@echo "sbom-scan: no $(SBOM_FAIL_ON)+ vulnerabilities found across $$(ls sbom/*.json | wc -l | tr -d ' ') SBOMs."

# ── Test ──────────────────────────────────────────────────────────────────────

test:
	cd $(GO_DIR) && go test ./...

# ── Coverage ─────────────────────────────────────────────────────────────────

coverage:
	@mkdir -p coverage
	cd $(GO_DIR) && go test -covermode=atomic \
		-coverprofile="$(CURDIR)/coverage/tools.out" ./...
	@cd $(GO_DIR) && go tool cover -func="$(CURDIR)/coverage/tools.out" | tail -1
	@echo "Per-package thresholds (COVERAGE_MINS):"
	@cd $(GO_DIR) && go run ./cmd/llz ci check-coverage \
		--profile "$(CURDIR)/coverage/tools.out" $(COVERAGE_MINS)
	@echo "Coverage profile written to coverage/tools.out"

# ── Instance smoke test ───────────────────────────────────────────────────────
# instance-test: the fast, LOCAL, no-cloud counterpart to release-e2e.yml. That
# workflow proves the template by standing up a REAL LKE-Enterprise cluster
# (instantiate → provision → validate → destroy) — slow and billable. This target
# runs only the parts that need no cloud: `copier copy` renders instance-template/
# into the build dir via the REAL instantiation path (so it catches the
# <@ token @> substitution bugs the release-e2e raw-cp hoist silently passes),
# then validates the rendered instance offline — no unrendered tokens, the
# load-bearing files present, and `terraform validate` on every rendered TF root
# (git:: module sources rewritten to the in-repo terraform-modules/, same trick as
# tf-validate-roots). Stands up NO cluster. Set SKIP_TF=1 to skip terraform.
# (The output dir is $${INSTANCE_TEST_DIR:-.instance-test}, read from the
# ENVIRONMENT by instance-test.sh. There is deliberately no `INSTANCE_TEST_DIR ?=`
# here: make does not export ordinary variables to recipes, so the assignment that
# used to sit on this line was dead — the script's own default always governed.)
instance-test: scaffold-check
	template-scripts/ci/instance-test.sh

# scaffold-check: scaffold a throwaway env via `llz env add` and assert the
# per-env scaffold is correct (no leftover `your-env`, required per-env files
# present, every apl-values/<env>/values.yaml renders through templatefile()).
# Catches the class of bug that only surfaced in Release-E2E before. No cloud;
# all artifacts removed on exit. Set SKIP_TF=1 to skip the templatefile render.
# Depends on `llz` so the scaffolder (bin/llz env add) is built first.
scaffold-check: llz
	template-scripts/ci/scaffold-render-check.sh

# llz-functional: drive the BUILT llz binary like an adopter and assert on real
# behaviour (vs the in-process unit tests, which stub the shell-out). Section A —
# basic commands (version/help/completion/env list/validation) — is offline and
# always runs. Section B exercises the documented INSTALL FLOW (docs/quickstart.md
# §2) against a real published release: `gh release download` + checksum, the
# authenticated `curl` against the private repo, and `llz self-update`'s
# download→verify→replace. Section B needs `gh` authenticated to the template repo
# (CI: GITHUB_TOKEN) and SELF-SKIPS when it isn't, so `make llz-functional` still
# runs section A offline. Set LLZ_FUNCTIONAL_NET=1 to require section B.
#
# `make lint` (the pre-commit gate) runs this with LLZ_FUNCTIONAL_NET=0 when the
# llz CLI source or this script changed — section A only, so the commit-time check
# stays offline + fast; the network install-flow is gated by release-e2e.yml.
llz-functional: llz
	template-scripts/ci/llz-functional.sh

# reap-orphans: the single manual entrypoint for clearing leaked Linode
# resources from failed/cancelled cluster cycles (the backlog that makes a fresh
# cluster-create HANG; `llz ci preflight` points here). Wraps the native
# `llz reap`, which sweeps clusters (if CLUSTER_LABEL) -> firewall -> NodeBalancers
# -> VPCs -> Volumes in dependency order. DRY-RUN by default; CONFIRM=yes to
# delete. NOT for routine teardown — CI uses the cluster-scoped `llz ci
# reap-volumes` / `reap-nodebalancers` sweeps instead.
reap-orphans: llz
	@LINODE_TOKEN='$(LINODE_TOKEN)' bin/llz reap --region '$(REGION)' --cluster-label '$(CLUSTER_LABEL)' $(if $(filter yes,$(CONFIRM)),--yes)

# ── Clean ─────────────────────────────────────────────────────────────────────

clean:
	cd $(GO_DIR) && go clean
	@rm -rf coverage sbom
