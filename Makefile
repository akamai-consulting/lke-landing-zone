SHELL := /bin/bash

.PHONY: help \
        build build-tools llz \
        fmt fmt-check vet shellcheck audit update tidy sbom gitleaks \
        sbom-go sbom-terraform sbom-kubernetes sbom-scan \
        chart-pin-guard chart-version-guard \
		setup-go-sole-site dependabot-coverage mutable-tag-guard provider-lock-guard tf-fmt tf-fmt-check tf-lint tf-validate tf-validate-roots checkov at-rest-guard managed-lock-check render-charts k8s-lint k8s-validate chart-guards prom-rules-check helm-repos helm-lint-real-values helm-lint-charts helm-dep-lock-check argocd-rendered-apps-check externalsecret-paths-check credential-coverage-guard wave-health-guard  mesh-egress-guard default-deny-egress  untestable-loc-check core-surface-check version-pins-check k8s-minor-coherence actions-lint  template-manifest-check docs-guard source-ref-guard symbol-ref-guard coverage-bank lint lint-k8s lint-tf \
        test coverage clean \
        instance-test upgrade-test scaffold-check llz-functional reap-orphans \
        install-tools install-syft install-trivy install-gitleaks

KUBECTL_VERSION  := 1.34.10

# Both forms are checked against dockerfiles/Dockerfile's ARG block by
# version-pins. That was NOT true when this line was added: the guard's separator
# was a single-character class, so it saw `=` and missed Make's `:=` — including
# KUBECTL_VERSION above, the one restatement living in the very file that declares
# the gate. Fixed in reArgRestatement; either spelling is safe now.
ACTIONLINT_VERSION = 1.7.7

# The Go module that holds the host-side tooling: tools/ (the `llz` CLI). The
# firewall-cidrs / firewall-controller commands moved to the private
# lke-landing-zone-internal repo.
GO_DIR := tools
# Bounded retry wrapper for flaky network fetches (helm index refreshes / chart
# pulls) — a transient upstream 5xx/DNS blip shouldn't fail a build. See the script.
RETRY := template-scripts/ci/with-retry.sh

# Per-package minimum statement coverage, as <pkg-suffix>=<percent> entries.
# pkg-suffix matches the END of a Go import path (internal/cli -> .../tools/internal/cli).
# `make coverage` fails the build if any listed package drops below its floor.
# It's a ratchet: bump a floor UP as that package's coverage improves, never
# down. Override on the CLI, e.g. `make coverage COVERAGE_MINS="internal/cli=20"`.
#
# A `#` COMMENT CANNOT GO INSIDE THIS LIST. The entries are backslash-continued,
# and a comment consumes the rest of its line INCLUDING the continuation — so an
# annotation next to one floor silently truncates every floor after it, and
# `make coverage` goes GREEN because the packages it stopped knowing about are the
# ones it stopped checking. It has happened twice, once dropping 123 packages to
# 115. Per-floor reasoning goes HERE, above the assignment, never inside it.
#
# THESE ARE PACKAGE-LOCAL NUMBERS. `go test -coverprofile` credits coverage to the
# package under test, not the package exercised, so a low floor on a freshly
# extracted package usually means "its tests are elsewhere" rather than "it is
# untested". Set a floor from the number this target prints, and say which case it
# is in a note here.
#
# `make coverage-bank` BANKS THE LOCAL READING, which on macOS/arm64 runs up to
# ~1pp above what CI (linux/amd64) measures for the same commit — enough to bank a
# floor that has never once passed the gate. After banking: revert every floor for
# a package your diff added no test to (the raise is measurement noise), and knock
# the rest ~1.5pp below the local number. A floor is a promise about the GATING
# environment, so that is the environment allowed to set it.
#
# internal/extensions/lifecycle/reconciler = 69, NOT 70, and this is not a floor
# lowered to dodge a regression. Its coverage is SCHEDULING-DEPENDENT: reconcile.go
# starts the metrics and health servers in goroutines, and whether they are
# scheduled before the test's context cancels decides whether three blocks are
# recorded as covered. Measured 70.3% at GOMAXPROCS 1 and 8, oscillating 69.9/70.3
# at 2; CI has read both 70.1 and 69.6. A floor of 70 sits inside that variance —
# a coin flip, not a promise. The real fix is to make the test wait until the
# health endpoint answers before cancelling, then raise this back.
COVERAGE_MINS := \
	internal/cli=71 \
	internal/cli/deps=41 \
	internal/extensions/lifecycle/brownfield=80 \
	internal/extensions/lifecycle/upstreamupdates=84 \
	internal/extensions/guards/dependabotcoverage=84 \
	internal/extensions/guards/callerperms=85 \
	internal/extensions/guards/runinjection=92 \
	internal/extensions/guards/secretscope=85 \
	internal/extensions/guards/defaultdeny=82 \
	internal/extensions/guards/budget=87 \
	internal/extensions/guards/chartguard=71 \
	internal/extensions/guards/k8sminorcoherence=99 \
	internal/shared/cli=98 \
	internal/extensions/assertions/assertnetwork=52 \
	internal/extensions/assertions/assertplatform=53 \
	internal/extensions/assertions/assertreconciler=82 \
	internal/extensions/assertions/assertregistry=62 \
	internal/extensions/lifecycle/atrest=89 \
	internal/shared/clusterspec=88 \
	internal/extensions/lifecycle/clusteraccess=71 \
	internal/shared/cigate=33 \
	internal/extensions/lifecycle/converge=75 \
	internal/extensions/lifecycle/healthsla=82 \
	internal/shared/color=86 \
	internal/extensions/guards/docsguard=71 \
	internal/shared/extension=96 \
	internal/shared/extension/registry=93 \
	internal/shared/harborauth=57 \
	internal/shared/health=95 \
	internal/shared/kube=86 \
	internal/shared/linode=83 \
	internal/shared/metrics=100 \
	internal/shared/guardkit=100 \
	internal/shared/guardwalk=60 \
	internal/extensions/lifecycle/objenc=52 \
	internal/extensions/lifecycle/environments=40 \
	internal/extensions/lifecycle/openbao=53 \
	internal/shared/pathglob=93 \
	internal/shared/promwire=92 \
	internal/extensions/lifecycle/promote=90 \
	internal/extensions/guards/credcoverage=87 \
	internal/extensions/assertions/configreadiness=53 \
	internal/shared/instancelayout=55 \
	internal/shared/yamledit=89 \
	internal/shared/kubectlprobe=77 \
	internal/shared/tfbin=90 \
	internal/shared/preflight=100 \
	internal/extensions/lifecycle/reconcilelanes=79 \
	internal/shared/s3sig=100 \
	internal/shared/shquote=100 \
	internal/extensions/assertions/sustain=55 \
	internal/extensions/lifecycle/teardown=47 \
	internal/extensions/assertions/tokeninv=74 \
	internal/shared/terraform=100 \
	internal/extensions/assertions/volumes=85 \
	internal/extensions/guards/wavehealth=82 \
	internal/extensions/lifecycle/tofudriver=25 \
	internal/extensions/assertions/assertobs=68 \
	internal/extensions/assertions/assertsecrets=65 \
	internal/shared/keycloak=49 \
	internal/extensions/assertions/assertidentity=24 \
	internal/extensions/lifecycle/deliverdocs=93 \
	internal/verbs/argodiag=81 \
	internal/extensions/guards/plaintext=90 \
	internal/extensions/lifecycle/chartpublish=55 \
	internal/extensions/assertions/manifestguard=73 \
	internal/extensions/lifecycle/assertobjstore=23 \
	internal/extensions/lifecycle/gameday=26 \
	internal/verbs/recondiag=60 \
	internal/verbs/phasetiming=62 \
	internal/verbs/doctor=86 \
	internal/extensions/lifecycle/kyverno=84 \
	internal/verbs/mutate=81 \
	internal/extensions/lifecycle/releasepublish=67 \
	internal/extensions/lifecycle/statepassphrase=74 \
	internal/shared/ghsecret=60 \
	internal/extensions/lifecycle/render=62 \
	internal/verbs/upgrade=33 \
	internal/verbs/newinstance=79 \
	internal/extensions/guards/pincoherence=94 \
	internal/verbs/lint=37 \
	internal/shared/copier=69 \
	internal/verbs/onboard=13 \
	internal/extensions/assertions/templatecommit=83 \
	internal/verbs/selfupgrade=57 \
	internal/extensions/assertions/buildpreflight=94 \
	internal/extensions/lifecycle/branchpolicy=34 \
	internal/extensions/assertions/reachability=37 \
	internal/extensions/lifecycle/firewall=68 \
	internal/extensions/guards/meshegress=52 \
	internal/extensions/guards/coverageguard=84 \
	internal/extensions/guards/cosignguard=75 \
	internal/extensions/guards/monitoringlabel=66 \
	internal/extensions/guards/setupgosite=79 \
	internal/extensions/guards/mutabletags=96 \
	internal/extensions/guards/providerlock=83 \
	internal/extensions/assertions/upgradeplan=85 \
	internal/extensions/guards/sourceref=87 \
	internal/extensions/guards/workflowshells=71 \
	internal/shared/answers=87 \
	internal/shared/llzver=96 \
	internal/shared/objstore=49 \
	internal/shared/openbao=79 \
	internal/shared/envtopology=69 \
	internal/shared/instanceresolve=93 \
	internal/shared/portfwd=93 \
	internal/shared/tokenprobe=44 \
	internal/shared/credtargets=83 \
	internal/shared/envreq=30 \
	internal/shared/manifest=88 \
	internal/shared/gitcmd=100 \
	internal/shared/envdef=54 \
	internal/shared/charty=95 \
	internal/shared/capability=94 \
	internal/shared/ghapi=89 \
	internal/shared/templateid=80 \
	internal/extensions/lifecycle/bootstrapcluster=61 \
	internal/extensions/assertions/seedspecial=85 \
	internal/shared/tfvars=56 \
	internal/extensions/guards/mtlsguard=91 \
	internal/extensions/guards/versionpins=84 \
	internal/extensions/assertions/assertsuite=73 \
	internal/extensions/guards/templatemanifest=93 \
	internal/shared/ghcli=42 \
	internal/extensions/lifecycle/reconciler=69 \
	internal/shared/ghgitdata=79 \
	internal/extensions/lifecycle/identityconfig=58 \
	internal/extensions/lifecycle/harbor=77 \
	internal/shared/baoread=79 \
	internal/shared/ghaout=81 \
	internal/extensions/lifecycle/credrotate=63 \
	internal/extensions/lifecycle/database=64 \
	internal/shared/proc=57

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
	@echo "  coverage-bank   raise each COVERAGE_MINS floor to what its package now measures"
	@echo "  clean           Remove build + coverage artifacts"
	@echo
	@echo "Terraform targets:"
	@echo "  tf-fmt          tofu fmt (auto-fix formatting)"
	@echo "  tf-fmt-check    tofu fmt -check (CI-safe, no writes)"
	@echo "  tf-lint         tflint — Terraform best-practice rules (.tflintrc.hcl)"
	@echo "  tf-validate     terraform validate — syntax + type checking (inits each module first)"
	@echo "  checkov         Checkov IaC security scan across all Terraform modules"
	@echo "  at-rest-guard   every TF root encrypts state; every node pool/volume sets disk encryption (ADR 0007 (state encryption))"
	@echo "  docs-guard      doc drift: llz FLAGS, gh workflow-run inputs, and links resolve"
	@echo "  source-ref-guard  stale tools/ path literals in prose, comments and error strings"
	@echo "  symbol-ref-guard  stale pkg.Symbol references in prose and Go comments"
	@echo "  setup-go-sole-site  workflows must set up Go via ./.github/actions/setup-llz, never a second setup-go pin"
	@echo "  dependabot-coverage  every dependency manifest is scanned by dependabot.yml, or excluded with a reason"
	@echo "  mutable-tag-guard  build-images.yml may publish :latest / :<version> only from the default branch"
	@echo "  provider-lock-guard delivered .terraform.lock.hcl pins satisfy the shipped provider constraints"
	@echo "  k8s-minor-coherence  lint.yml's kind node image must run the k8s minor we deploy to LKE-E"
	@echo
	@echo "Kubernetes targets:"
	@echo "  k8s-lint        kube-linter — k8s best-practice checks (.kube-linter.yaml)"
	@echo "  mtls-wiring-guard  OpenBao consumers must mount the mTLS material they read (ADR 0010)"
	@echo "  plaintext-guard  registry gate on unencrypted in-cluster hops (ADR 0010)"
	@echo "  credential-coverage-guard  every workflow secret is measured, or registered as an exemption"
	@echo "  k8s-validate    kubeconform — schema validation against k8s $(KUBECTL_VERSION)"
	@echo "  prom-rules-check  promtool check rules — PromQL syntax + rule structure"
	@echo "  helm-lint-charts  helm lint --strict + template every first-party chart"
	@echo "  helm-lint-real-values  hard dep-build + namespaced render of the OpenBao chart (lint --strict is helm-lint-charts' job)"
	@echo "  helm-dep-lock-check  verify committed Chart.lock files match Chart.yaml dependency declarations"
	@echo "  chart-guards    run BOTH chart guards (version bump + Argo pin realignment) — a bump needs both"
	@echo "  llz-gates       ALL of them at once — every gate binding the extension registry"
	@echo "                  declares and can drive, in one process. This is what lint-k8s runs."
	@echo "                  For ONE gate: llz ci gates --only <extension|command>. Four names"
	@echo "                  used to be listed below as make targets and no longer are —"
	@echo "                  wave-dependency-guard, monitoring-label-guard,"
	@echo "                  dropped-apiversions-check and placeholder-guard now run only here."
	@echo "  argocd-rendered-apps-check  render overlays and reject duplicate ArgoCD Helm parameters"
	@echo "  externalsecret-paths-check  validate ExternalSecret refs and OpenBao policy coverage"
	@echo "  wave-health-guard           negative-sync-wave kinds must be health-safe (PR #142 wedge class)"
	@echo "  mesh-egress-guard           no NetworkPolicy egress to a STRICT-mesh namespace (harbor) from outside it"
	@echo "  default-deny-egress         no pod is policed into total silence (egress enforced, none granted)"
	@echo "  untestable-loc-check  fail when inline-bash/shell/python logic exceeds .untestable-budget.yaml"
	@echo "  core-surface-check    fail when Go logic in package main exceeds .core-surface-budget.yaml (ADR 0014)"
	@echo "  actions-lint    actionlint — GitHub Actions workflow linting"
	@echo "  lint            Changed-file linters; LINT_ALL=1 runs the local mirror of the"
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
	@echo "  upgrade-test    Day-2 half: scaffold at each of the last 3 releases, then run"
	@echo "                  the real llz upgrade to HEAD. Asserts each runs unattended,"
	@echo "                  keeps every answer it does not own, moves the pin, leaves no"
	@echo "                  merge artifacts, and ends up IDENTICAL to a fresh scaffold."
	@echo "                  Offline; self-skips without copier or tags. --depth N to vary."
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
	go install golang.org/x/vuln/cmd/govulncheck@latest && go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
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
# NOTE: this is an explicit list, not a terraform-modules/* glob, so a NEW module
# is unguarded until it is added here — tf-fmt-check, tf-lint, tf-validate and
# checkov all iterate it. llz-databases shipped needing a `sensitive = true` its
# absence from this list hid (the provider marks root_username sensitive, which is
# a hard validate error only when the module is validated AS a root — which is
# exactly what tf-validate does). Add every new module.
TF_DIRS := $(wildcard terraform-modules/llz-cluster \
                      terraform-modules/llz-object-storage \
                      terraform-modules/llz-databases)

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
# the native port of the former linting-and-validation/
# check-prometheus-rule-crds.py; uses the PATH llz when present (the CI images
# bake it), else builds from source.
# LLZ_CI — invoke one `llz ci <verb>`. Nine targets stamped out this same
# if/else, and getting it wrong is not cosmetic: the two branches had already
# drifted, and the PATH branch silently wins on a workstation where `llz` is
# whatever you last installed — so a guard can report a pass that says nothing
# about your working tree.
#
#   $(1) verb + flags
#   $(2) further flags
#
# Both branches cd into $(GO_DIR) and take $(2), so there is no "args that work
# from the repo root" vs "args that work from tools/" convention to remember. They
# used to differ, with the PATH branch silently DROPPING $(2); it survived only
# because every --root .. happened to match the repo-root default.
#
# PATH-FIRST IS RIGHT FOR CI: the ci-kubernetes / ci-terraform images bake llz and
# carry no Go toolchain, so `go run` cannot fire there at all. Two conditions
# override it, because both make the installed binary answer a different question
# from the one asked:
#
#   * $(GO_DIR) HAS UNCOMMITTED CHANGES — the installed binary would answer for
#     code you did not write. Announcing the branch was not enough on its own: it
#     scrolls past in a wall of lint output, and the wrong answer it labels is
#     indistinguishable from a right one. CI is unaffected; a clean checkout takes
#     the PATH branch exactly as before.
#   * A `gates …` INVOCATION, whatever the tree's state. The gate driver's subject
#     IS the extension registry in this tree — which gates exist, what each is
#     named, what its command does — so an installed binary answers from the
#     registry IT was built with: a gate this tree adds is "unknown" to it, and a
#     gate this tree CHANGED runs as the older implementation. That shipped once as
#     `make LINT_ALL=1 lint` dying on `unknown flag: --only` from an older llz on
#     PATH, invisible during development because the tree is dirty while you work.
#     Four targets had the right behaviour via an explicit `export
#     LLZ_FORCE_SOURCE := 1` — the shape that rots, since the tenth target added
#     would have forgotten it. Deciding it from the invocation cannot be forgotten.
#
# LLZ_FORCE_SOURCE=1 forces source unconditionally — what you want locally the
# moment you have touched tools/.
define LLZ_CI
	@set -e; \
	dirty=""; \
	if git -C . rev-parse --git-dir >/dev/null 2>&1; then \
		dirty="$$(git -C . status --porcelain -- $(GO_DIR) 2>/dev/null | head -1)"; \
	fi; \
	driven=""; \
	case '$(1)' in gates*) driven=1;; esac; \
	if [ -z "$$LLZ_FORCE_SOURCE" ] && [ -z "$$driven" ] && [ -z "$$dirty" ] && command -v llz >/dev/null 2>&1; then \
		echo "[llz: $$(command -v llz) $$(llz version 2>/dev/null | head -1) — NOT your working tree; LLZ_FORCE_SOURCE=1 to build from source]"; \
		cd $(GO_DIR) && llz ci $(1) $(2); \
	else \
		if [ -n "$$dirty" ]; then \
			echo "[llz: built from source — $(GO_DIR) has uncommitted changes, so the installed binary would answer for code you did not write]"; \
		elif [ -n "$$driven" ]; then \
			echo "[llz: built from source — a gate-driver run answers from THIS tree's registry, not the installed binary's]"; \
		else \
			echo "[llz: built from source]"; \
		fi; \
		cd $(GO_DIR) && go run ./cmd/llz ci $(1) $(2); \
	fi
endef

# --rules-dir is THE WHOLE platform-apl TREE, not the one directory named after
# the concern. check-prom-rules walks a root and filters by `kind: PrometheusRule`,
# so pointing it at a subdirectory did not scope the check to the interesting
# rules — it scoped it to the rules that happened to live in the folder someone
# thought of, and llz-reconciler's PrometheusRule has never been in it. Nineteen
# rules, including every alert on the reconciler's own gauges, were outside the
# corpus of the gate that validates PromQL.
#
# A gate with a hole in its corpus is the vacuous-pass shape one level up: it
# passes, it prints a count, and the count is of the files it was pointed at
# rather than the files that exist. TestEveryPrometheusRuleIsInTheCheckedCorpus
# is the other half — it fails when a PrometheusRule lands outside whatever root
# this line names.
prom-rules-check:
	$(call LLZ_CI,check-prom-rules,--rules-dir ../platform-apl)

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
	$(call LLZ_CI,gates --only argocd-rendered-apps,)

# externalsecret-paths-check: `llz ci externalsecret-paths` (the native port of
# the former validate-externalsecret-paths.py). Uses the PATH llz when present
# (the CI images bake it); otherwise builds from source via the Go toolchain.
externalsecret-paths-check: export RENDER_DIR := $(RENDER_DIR)
externalsecret-paths-check: render-charts
	$(call LLZ_CI,gates --only externalsecret-paths,)

# wave-health-guard: `llz ci wave-health-guard` — the PR #142 wedge-class gate.
# Argo sync waves gate on per-resource health; a health-checked kind at a
# negative wave that can be not-Ready on a fresh cluster wedges the
# platform-bootstrap sync before OpenBao (wave 0). Every negative-wave kind in
# platform-apl/manifest/ + platform-apl/components/ must be health-inert or
# backed by a resource.customizations.health override in apl-values/values.yaml.
wave-health-guard:
	$(call LLZ_CI,gates --only wave-health-guard,)

# mtls-wiring-guard: `llz ci mtls-wiring-guard` — asserts that a pod declaring
# OPENBAO_ADDR mounts the TLS material inClusterBaoHTTPClient() reads, that every
# TLS Secret it mounts has a Certificate creating it, and that OPENBAO_SKIP_VERIFY
# stays deleted. Exists because dropping the client-cert mount used to pass every
# other gate while leaving the pod unable to reach OpenBao (ADR 0010).
mtls-wiring-guard:
	$(call LLZ_CI,gates --only mtls-wiring-guard,)

# plaintext-guard: `llz ci plaintext-guard` — the drift gate on UNENCRYPTED
# in-cluster hops (docs/adr/0010-in-cluster-mtls.md). Every `scheme: http`
# scrape, `insecureSkipVerify: true`, in-cluster http:// URL (fully qualified or
# the short svc.namespace / svc forms), Istio mesh policy accepting cleartext
# (PeerAuthentication PERMISSIVE / DestinationRule tls.mode DISABLE), and Go
# InsecureSkipVerify must be registered in plaintextAllowed with a reason and an
# owner. Unregistered hits fail; so do registry entries whose hop is gone, so the
# list cannot rot into a rubber stamp.
plaintext-guard:
	$(call LLZ_CI,gates --only plaintext-guard,)

# credential-coverage-guard: `llz ci credential-coverage-guard` — the drift gate on
# credential OBSERVABILITY. Every `secrets.NAME` an instance workflow consumes must
# be measured by one of the single-pane feeds (expiry via ghPATTargets or the Linode
# account enumeration; GitHub write time via ghSecretTargets) or be registered in
# credCoverageExempt with a kind and a reason. Coverage is DERIVED from those lists,
# so the only way to satisfy it for a real credential is to measure it. Exists
# because OPENBAO_SEAL_KEY — the at-rest key for every other credential in the
# platform — sat off the pane by omission, and nothing in the repo noticed.
#
# FROM SOURCE, same reason as managed-lock-check: it compares the working tree
# against Go lists in the working tree, and the prebuilt image binary is built from
# the merge-base (so it lacks this verb on the PR that introduces it).
credential-coverage-guard: export LLZ_FORCE_SOURCE := 1
credential-coverage-guard:
	$(call LLZ_CI,gates --only credential-coverage-guard,)

# at-rest-guard: `llz ci at-rest-guard` — the drift gate on ENCRYPTION AT REST for
# Terraform-declared resources (docs/adr/0007-terraform-state-encryption.md). Every
# root must declare `terraform { encryption }` (state holds kubeconfig_raw and every
# Managed Postgres root_password in the clear), every node pool must set
# disk_encryption, every linode_volume must set encryption. All three are ForceNew:
# decided at create, immutable after, so a gate is the only place to catch them.
# The ADR 0007 (state encryption) phase-1 unencrypted fallback is the one registered residue, and it
# carries an exit condition rather than living as a comment in four files.
#
# FROM SOURCE, same reason as credential-coverage-guard: the prebuilt image binary
# is built from the merge-base, so on the PR that introduces this verb it does not
# exist there — the gate would fail with `unknown command` rather than run.
at-rest-guard: export LLZ_FORCE_SOURCE := 1
at-rest-guard:
	$(call LLZ_CI,at-rest-guard,--root ..)

# mesh-egress-guard: `llz ci mesh-egress-guard` — the harbor-reconciler mesh class.
# apl-core runs platform namespaces (harbor) under Istio STRICT mTLS; a pod OUTSIDE
# that mesh can't reach a Service inside it (dropped at the sidecar, not by
# NetworkPolicy). Flags any NetworkPolicy egress to a STRICT-mesh namespace from a
# different namespace — the batch-5 harbor reconciler's mistake, caught at PR time
# instead of two ~50-minute e2e cycles.
#
# Depends on render-charts: the first-party charts' own NetworkPolicies are only
# visible once rendered (walkManifests skips templates/, and their target
# namespaces are Helm values), and the guard hard-fails on a missing rendered tree
# rather than passing green over a corpus it never saw.
mesh-egress-guard: render-charts
	$(call LLZ_CI,gates --only mesh-egress-guard,)

# default-deny-egress: `llz ci default-deny-egress` — the openbao-cert-watcher
# class. NetworkPolicies are additive with no deny rule, so a namespace-wide
# `podSelector: {}` + `policyTypes: [Egress]` starts policing EVERY pod in the
# namespace and any pod no companion allow selects reaches nothing at all — not
# DNS, not the apiserver. The pod stays 1/1 Running with healthy endpoints, so
# there is no status, no event and no restart for anything to observe.
#
# The watcher shipped that way: llz-openbao-platform's default-deny polices the
# namespace, its one companion selects `app.kubernetes.io/name: openbao`, and the
# watcher carries `openbao-cert-watcher`. Its kubectl poll was dropped for the
# life of every cluster, so at certificate renewal nothing restarted OpenBao.
#
# Same `: render-charts` as mesh-egress-guard, and for a sharper reason: the
# default-deny that strands a pod lives in a CHART and the pod lives in
# platform-apl/, so without the rendered tree every pod reads as UNPOLICED and the
# guard passes over precisely the class it exists for. It hard-fails on a missing
# rendered tree rather than taking that pass.
default-deny-egress: render-charts
	$(call LLZ_CI,gates --only default-deny-egress,)

# untestable-loc-check: the design-principle gate. Fails when inline workflow
# bash / shell / python logic exceeds the budget in .untestable-budget.yaml —
# the signal to convert logic into the unit-tested llz CLI rather than pile more
# untestable shell into CI. Pure Go + a config file, so it runs anywhere (no
# rendered charts needed). Budgets ratchet DOWN as code is converted.
untestable-loc-check:
	$(call LLZ_CI,gates --only untestable-loc,)

# core-surface-check: the counterweight to untestable-loc-check (ADR 0014). That
# gate names the llz CLI as the destination for converted logic but caps
# nothing there, so package main accretes — 236 non-test files, 130 of them
# ci_*.go. This one budgets the destination: Go logic lines in package main,
# from .core-surface-budget.yaml. Satisfy it by extracting to
# tools/internal/<pkg> (ADR 0013) or moving the capability out to an extension
# (issue #10) — never by raising the budget. Ratchets DOWN, same as its sibling.
#
# `make lint` fires this and untestable-loc-check from ONE changed-file
# condition, so a Go-only change also runs the sibling and a workflow-only change
# also runs this. Both are pure Go over a config file and take well under a
# second; splitting the condition would have cost recipe lines in a category with
# one line of headroom left, to save nothing measurable.
core-surface-check:
	$(call LLZ_CI,gates --only core-surface,)

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

# chart-pin-guard: assert every Argo CD first-party chart pin (apl-values
# targetRevision + llz-argo-bootstrap-apps component version) matches the chart's
# local kubernetes-charts/<chart>/Chart.yaml version. A pin the registry never
# received 404s at Argo sync time — on a cold bootstrap that silently strands the
# support-plane app (llz-openbao namespace never created) and times out the
# OpenBao bootstrap. Decision logic is unit-tested Go; this is thin glue.
chart-pin-guard:
	$(call LLZ_CI,gates --only chart-pin-guard,)

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
# (templatization-plan.md §5). `helm lint --strict` enforces schema +
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

# actions-lint: actionlint over THIS repo's workflows.
#
# THE FALLBACK IS NOT COSMETIC — without it this target is `command not found`
# wherever actionlint is not preinstalled, and that is most places. actionlint is
# copied into the devcontainer stage of dockerfiles/Dockerfile ONLY; neither
# ci-tofu nor ci-kubernetes ships it. A comment on the lint-k8s line asserted the
# opposite ("the CI image already ships actionlint") and put this target on that
# line, which made the Kubernetes lint job exit 127 the first time CI ran it.
# Same shape as staticcheck below, and for the same reason.
actions-lint:
	@if command -v actionlint >/dev/null 2>&1; then \
	  actionlint .github/workflows/*.yml; \
	else \
	  echo "actionlint not on PATH — falling back to 'go run' (make install-tools installs it)"; \
	  cd $(GO_DIR) && go run github.com/rhysd/actionlint/cmd/actionlint@v$(ACTIONLINT_VERSION) ../.github/workflows/*.yml; \
	fi

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
# GATES DO NOT GET A TARGET HERE. Every guard used to be a separate `llz ci
# <verb>` shell-out listed in these variables, so the Makefile and the extension
# registry each held a list of which guards exist — and the registry's drifted
# from its own table. `llz ci gates` is now the single source of that truth: the
# gates it drives are whatever the declarations say, so adding a guard is a
# registry edit and nothing else.
#
# TO RUN ONE WHILE ITERATING, use `llz ci gates --only <extension|command>`, with
# the flags coming from the model rather than a second copy here. A surviving gate
# target is one line of that plus whatever Makefile knowledge the driver has no
# business holding — a render-charts prerequisite, an LLZ_FORCE_SOURCE export.
# Add a target back only when something NAMES it (a workflow step, a guard's own
# remediation message), and make it a `--only` line when you do.
#
# WHAT CANNOT MOVE: k8s-lint (kube-linter), k8s-validate (kubeconform), the three
# helm-lint targets, and prom-rules-check — which needs promtool on PATH and is an
# ASSERTION binding rather than a gate. External tools are not gates and the driver
# has no business pretending otherwise.
LINT_K8S := k8s-lint k8s-validate prom-rules-check \
            helm-lint-charts helm-lint-real-values \
            helm-dep-lock-check
LINT_TF := tf-lint checkov at-rest-guard tf-validate-roots

# llz-gates: every gate binding the extension registry declares AND can drive.
#
# render-charts is a prerequisite because several gates need it — the
# chart-shipped NetworkPolicies and the openbao ServiceMonitor are only real YAML
# once rendered. The driver has no notion of per-gate prerequisites, so the
# requirement is hoisted to the whole set; running it unnecessarily costs a render
# nobody minds. That hoist is also why a `--only` target still carries its own
# `: render-charts` — the driver cannot know that mesh-egress needs a rendered
# tree and core-surface does not.
#
# LLZ_FORCE_SOURCE BECAUSE THE DRIVER MUST RUN THE WORKING TREE'S GATE SET. An
# installed llz runs the gates IT knows, so a PR adding or changing a gate would
# be judged by a binary that predates it — silently, with a clean result. LLZ_CI's
# dirty-tree detection does not cover this: that detects an UNCOMMITTED tools/,
# and a PR's gate changes are committed. Building once here also pays the source
# build once for the whole suite instead of per gate.
llz-gates: export LLZ_FORCE_SOURCE := 1
llz-gates: export RENDER_DIR := $(RENDER_DIR)
llz-gates: render-charts
	$(call LLZ_CI,gates,)

# CI job entrypoints — one target per lint.yml container job.
# llz-gates IS NAMED HERE EXPLICITLY, not folded into LINT_K8S, and the
# distinction is load-bearing: LINT_K8S is the CHART-tool list the recipe below
# runs on a kubernetes-charts change, while the gate suite must run whenever this
# job runs at all. `make lint-k8s` is the only entry point any CI job calls, so a
# gate absent from this line is a gate that exists, passes review, and never runs
# — which collapsing the two once already caused.
#
# `actions-lint` IS HERE FOR THAT REASON. It lints THIS repo's own
# .github/workflows/*.yml; the only actionlint that otherwise runs is inside
# instance-test.sh, over the RENDERED INSTANCE's workflows — a different tree — so
# the workflows that decide what CI does were checked by nothing but the
# pre-commit hook, which lives in .git/hooks: per-clone, never committed, absent
# for a web edit and for Dependabot's own workflow bumps.
#
# IT RUNS IN lint.yml's `go-tests` JOB, NOT HERE, because ci-tofu and ci-kubernetes
# ship neither actionlint nor a Go toolchain (it is COPYed into the `devcontainer`
# stage only, which is what makes a Dockerfile grep look reassuring). Putting it on
# this line died on `actionlint: command not found` the first time CI ran it, and
# passed locally because a developer has it on PATH. `go-tests` is a host runner
# with Go, so the target's `go run` fallback resolves there. shellcheck stays here
# because ci-kubernetes genuinely does ship it.
lint-k8s: $(LINT_K8S) shellcheck llz-gates
#
# `tf-fmt-check` IS ON THIS LINE FOR THE SAME REASON, and it is the fourth of the
# same class. LINT_TF is tf-lint/checkov/at-rest-guard/tf-validate-roots — every
# terraform check EXCEPT formatting — so `tofu fmt -check` ran nowhere in CI and
# was enforced only by the pre-commit hook, which is per-clone and uncommitted.
# The Makefile's changed-file lint has always run `tf-fmt-check $(LINT_TF)`
# together on a .tf change; CI ran half the pair. The ci-tofu image this job uses
# already ships tofu, so it costs nothing.
lint-tf: $(LINT_TF) tf-fmt-check template-manifest-check managed-lock-check

# Assert .template-manifest classifies every scaffold file (managed/merge/owned),
# so the template-update tooling never has to guess about a new file.
# The subtree it checks (instance-template) is declared in registry/gates.go
# beside the gate it belongs to, not spelled out here.
#
# FROM SOURCE, because it classifies the WORKING TREE's scaffold against the
# WORKING TREE's .template-manifest and must therefore run the working tree's llz.
# LLZ_CI's PATH-first branch takes an `llz` from PATH whenever the tree is clean,
# and in the `terraform` CI job that is the binary BAKED INTO ci-tofu at
# image-build time — setup-llz runs there with no install-path, so it installs the
# Go toolchain without rebuilding. On a PR that ADDS this gate the stale binary
# fails loudly on `--only`; on a PR that CHANGES the classification logic it
# passes, having validated the new scaffold with the old rules.
template-manifest-check: export LLZ_FORCE_SOURCE := 1
template-manifest-check:
	$(call LLZ_CI,gates --only template-manifest,)

# Assert instance-template/.template-managed.lock still matches the template-owned
# .github/ files it covers. Editing a llz-*.yml body without re-running
# `llz ci managed-fresh --write` would ship a lock that every instance fails on,
# so catch it here instead. managed-fresh is NOT a driven gate (its Deps are
# assembled in the CLI layer — see undrivenGates in registry/gates.go), so unlike
# the target above it is called as a bare verb rather than through `gates --only`.
#
# FROM SOURCE, for the same reason as template-manifest-check above and a sharper
# one: this gate compares the WORKING TREE's scaffold against the WORKING TREE's
# lock, and LLZ_CI's PATH-first default would use the prebuilt image binary, which
# is built from the merge-base and does not even have this verb on the PR that
# introduces it.
#
# The recipe cds to $(GO_DIR) first, so `--root ../instance-template` is relative
# to tools/ — do not "simplify" it to the repo-root spelling.
managed-lock-check: export LLZ_FORCE_SOURCE := 1
managed-lock-check:
	$(call LLZ_CI,managed-fresh --root ../instance-template,)

# Assert every restatement of a tool version agrees with the Dockerfile ARG block
# (the declared single source of truth): the build-images matrix, lint.yml's env
# pins, this file's own KUBECTL_VERSION, and the CITofuTag/CIKubernetesTag
# constants that derive TF_IMAGE/KUBE_IMAGE. The Go constants sat on Terraform
# 1.9.8 after the other two moved to OpenTofu 1.12.5 — caught by hand then,
# caught here now.
#
# ...and asserts the INVERSE on lint.yml's `vars.TF_IMAGE`/`vars.KUBE_IMAGE`
# container fallbacks: those must name `:latest`. Gating them as pins made a
# version bump red-light Lint exactly once per bump, on an image build-images.yml
# had not published yet.
#
# FROM SOURCE, for the same reason as managed-lock-check: this compares the
# WORKING TREE against itself, and the prebuilt image binary is built from the
# merge-base (so it lacks this verb on the PR that introduces it).
version-pins-check: export LLZ_FORCE_SOURCE := 1
version-pins-check:
	$(call LLZ_CI,gates --only version-pins,)

# k8s-minor-coherence: `llz ci k8s-minor-coherence` — the kind cluster lint.yml
# server-side dry-runs against must run the Kubernetes MINOR we deploy.
#
# lint.yml pins kind's VERSION; if it does not also pin the NODE IMAGE, `kubectl
# apply --dry-run=server -f rendered/` — the only check here that asks a real API
# server whether the manifests are acceptable — runs against kind's default image
# rather than the minor the cluster root pins. An API removed in a later minor, or
# a field only a newer server validates, then passes green (#427).
#
# NOTHING ELSE CAN SEE IT: both pins are individually valid, only the RELATION
# between them is wrong — the same shape as version-pins, setup-go-sole-site and
# mutable-tag-guard — and it can be created by a change to neither site.
#
# FROM SOURCE, for version-pins-check's reason: it compares the working tree
# against itself, and the prebuilt image binary is built from the merge-base (so
# it lacks this verb on the PR that introduces it).
k8s-minor-coherence: export LLZ_FORCE_SOURCE := 1
k8s-minor-coherence:
	$(call LLZ_CI,gates --only k8s-minor-coherence,)

# docs-guard runs on a DOC change, yes — but also on a CLI or workflow-input
# change, which is what actually causes doc rot. A renamed flag makes a doc wrong
# without touching the doc, so scoping this to *.md would miss precisely the drift
# it exists to catch.
#
# docs-guard: validate every Markdown file against the repo it documents —
# `llz` commands + flags against the live cobra tree, `gh workflow run` inputs
# against the workflows' declared inputs, and relative links resolved BOTH in
# this tree and in the post-`deliver-docs` keep-set an adopter actually carries.
#
# Added after a full audit of the 104 Markdown files: of 30 defects found, the
# majority were mechanically detectable from the repo itself and had simply never
# been asked about. The expensive ones were all in the DELIVERED operator set,
# which is why the link half evaluates that keep-set specifically.
#
# FROM SOURCE, like version-pins-check: it compares the working tree against
# itself, so a prebuilt image binary built from the merge-base lacks the verb on
# the PR that introduces it — and, more importantly, would check the docs against
# an OLD CLI, which is the exact drift this is meant to catch.
docs-guard: export LLZ_FORCE_SOURCE := 1
docs-guard:
	$(call LLZ_CI,gates --only docs-guard,)

# source-ref-guard: `llz ci source-ref-guard` — every `tools/…` path literal in
# prose resolves to something that exists.
#
# THE GAP DOCS-GUARD LEAVES. That guard checks Markdown LINKS, `llz` flags,
# workflow dispatches and TOCs. A path written as PROSE — inside a sentence, an
# `#` comment in a chart or workflow, a shell header, a Go error string — is none
# of those, and nothing looked at one. The move of package main into
# tools/internal/** left ~90 such references naming files that no longer existed,
# three of them gone under every path; the worst read "add new seeds THERE" beside
# a directory deleted a campaign earlier. A dead link is a dead end, but a dead
# pointer with an instruction on it sends the reader somewhere wrong.
#
# FROM SOURCE, for docs-guard's reason one level over: it compares the working
# tree against ITSELF, so the merge-base binary would resolve this branch's
# references against the old tree and pass on exactly the moves that break them.
source-ref-guard: export LLZ_FORCE_SOURCE := 1
source-ref-guard:
	$(call LLZ_CI,gates --only source-ref-guard,)

# setup-go-sole-site: `llz ci setup-go-sole-site` — actions/setup-go may be named
# by .github/actions/setup-llz and by nothing else.
#
# THE COMPOSITE WAS EXTRACTED TO END A 13-SITE SWEEP, and a 14th site had already
# grown back. release-e2e-lane.yml's `llz functional` job hand-rolled setup-go at
# v7.0.0 while the composite sat at v6.5.0 — a full major apart — running the SAME
# functional script that llz-release.yml runs THROUGH the composite. So the two
# release gates built llz on two different toolchain actions, and the build flags
# the composite standardises (-buildvcs=false) were absent from one of them.
#
# NOTHING COULD SEE IT. actionlint validates each `uses:` in isolation and a
# correctly SHA-pinned action is textually identical whether or not it is the
# right one; the rule was written in .github/workflows/AGENTS.md ("`actions/setup-go`
# should appear nowhere else") and enforced by nobody. The defect is only visible
# as a RELATION between sites, which is the same shape version-pins exists for.
#
# FROM SOURCE for the usual reason: on the PR that introduces the verb, the
# merge-base image binary does not have it.
setup-go-sole-site: export LLZ_FORCE_SOURCE := 1
setup-go-sole-site:
	$(call LLZ_CI,gates --only setup-go-sole-site,)

# dependabot-coverage: `llz ci dependabot-coverage` — every dependency manifest in
# the tree is scanned by a .github/dependabot.yml entry, or listed in
# .dependabot-coverage.yaml with a reason.
#
# THREE PIN SETS WERE UNSCANNED AT ONCE and each looked correct where it sat
# (#502). For the github-actions ecosystem `directory: "/"` means .github/workflows
# plus a ROOT-level action.yml and nothing else — so the guard above, which
# consolidated the repo's only actions/setup-go pin into a composite action, moved
# that pin out of Dependabot's reach in the same stroke. It was a major version
# stale before anyone looked; `git log --author=dependabot -- .github/actions/`
# was empty. dockerfiles/ and terraform-modules/ had never been listed at all.
#
# THE FAILURE IS SILENCE. Dependabot reports nothing about a directory it was
# never asked to scan, and a config that omits an ecosystem is well-formed — so
# "no PRs this week" is what both working coverage and absent coverage look like.
# Only the relation between the tree and the config can be checked, which is the
# same shape as version-pins and setup-go-sole-site.
#
# FROM SOURCE for the usual reason: on the PR that introduces the verb, the
# merge-base image binary does not have it.
dependabot-coverage: export LLZ_FORCE_SOURCE := 1
dependabot-coverage:
	$(call LLZ_CI,gates --only dependabot-coverage,)

# mutable-tag-guard: `llz ci mutable-tag-guard` — build-images.yml may publish a
# MUTABLE tag (`:latest`, `:<version>`) only from the default branch's HEAD.
#
# build-images.yml's `workflow_dispatch` is deliberately NOT gated on the ref —
# release-e2e and e2e-instantiate drive it on feature branches — so without this
# guard every branch that runs an e2e republishes `:latest` and `:<version>` from
# its own content (#451). Three readers move with it at once: lint.yml's container
# fallback (this repo sets neither TF_IMAGE nor KUBE_IMAGE), `llz ci
# assert-image-fresh`, which reads the baked sha expecting the template ref's
# commit, and any instance that never pinned an image. The no-path-filter design
# at the top of build-images.yml exists solely to keep `:latest` == main's HEAD.
#
# NOTHING ELSE CAN SEE IT: each `--tag` is individually well-formed and the tag
# looks identical after it moves. Only the relation between the publish and the
# ref is checkable.
#
# FROM SOURCE for the usual reason: on the PR that introduces the verb, the
# merge-base image binary does not have it.
mutable-tag-guard: export LLZ_FORCE_SOURCE := 1
mutable-tag-guard:
	$(call LLZ_CI,gates --only mutable-tag-guard,)

# provider-lock-guard: the delivered .terraform.lock.hcl vs. the constraints the
# roots and modules ship — `llz ci provider-lock-guard`.
#
# THE FAILURE MODE. An instance commits no Terraform code: the roots are
# generated at every terraform op by the llz inside vars.TF_IMAGE, and
# terraform-iac-bootstrap/*/*.tf is gitignored. What it DOES commit is the
# provider lockfile, which .template-manifest classes `owned` — seeded once at
# scaffold time and never re-touched by an upgrade. So the CONSTRAINT ships in
# the image and the PIN sits in the adopter's repo, and nothing compared them.
#
# Raise linode past the shipped pin and a new adopter is fine, release-e2e is
# green (it force-pushes a fresh instantiation every run), and EVERY EXISTING
# INSTANCE is hard-blocked at `tofu init` — which the terraform-init composite
# runs with no -upgrade, so there is no recovery inside CI. Greenfield passes,
# brownfield breaks, and no lane can see the difference.
#
# FROM SOURCE for the usual reason: on the PR that introduces the verb, the
# merge-base image binary does not have it.
provider-lock-guard: export LLZ_FORCE_SOURCE := 1
provider-lock-guard:
	$(call LLZ_CI,gates --only guard-provider-lock,)

# symbol-ref-guard: the OTHER half of a reference — `llz ci symbol-ref-guard`.
#
# A path can be right while the symbol beside it is wrong, which is what a move
# between packages leaves behind once the compiler has forced every CALLER to be
# updated and left every COMMENT alone. The sweep that fixed this repo's paths
# shipped exactly that defect, naming the file a symbol had already left.
#
# It enforces the convention that makes it shippable against prose which
# documents its own history: `pkg.Symbol` is a LIVE pointer and must resolve,
# `pkg's Symbol` is where something used to be. Without that split every one of
# its first fifteen findings was a correct sentence, and a guard that fails on
# correct sentences is one that gets deleted.
#
# FROM SOURCE, like its sibling: it indexes the working tree's own exported
# surface, so a merge-base binary would judge this branch's references against
# the old API and pass on exactly the renames that break them.
symbol-ref-guard: export LLZ_FORCE_SOURCE := 1
symbol-ref-guard:
	$(call LLZ_CI,gates --only symbol-ref-guard,)

# NO PER-GATE TRIGGERS. This recipe once carried a hand-written `grep -qE` per
# gate — a copy of "which gate cares about which paths" sitting beside the gate
# that knows. The whole 24-gate suite runs in 3.8s, so selection bought nothing
# and cost a copy that can drift, and it had already drifted: docs-guard was
# wired in with a filter matching no Markdown, so the one change class it was
# built for never ran it. `llz-gates` now runs unconditionally instead.
#
# UNCONDITIONALLY MEANS BOTH BRANCHES. LINT_ALL=1 runs a fixed list and exits;
# the changed-file path falls through to `llz-gates` at the bottom. With the gate
# call only at the bottom, `make lint LINT_ALL=1` — documented in three places as
# running "every check" — was the one mode that skipped the entire gate suite.
# Roughly half of it (posture-plaintext, mesh-egress, mtls-wiring, guard-source-refs,
# guard-cosign-subject, guard-monitoring-labels, guard-manifests, wave-health,
# pin-coherence) is reachable ONLY through `llz-gates`.
#
# What stays conditional below is genuinely conditional: EXTERNAL tools
# (shellcheck, tflint, kube-linter, actionlint) and the two expensive ones
# (instance-test, coverage). Those are not gates and the registry has no opinion
# about them.

# lint-changed prints the file set `lint` decides from, one path per line. It is a
# target rather than an inline pipeline so it can be tested.
#
# EVERYTHING HERE ANSWERS ONE QUESTION: does this target ever say "nothing" when
# the answer is "something"? Every arm of `lint` sits behind that answer.
#
# ASK GIT FROM THE REPOSITORY ROOT. `git ls-files` reports CWD-relative paths and
# lists only the subtree; `git diff --name-only` reports repo-relative paths unless
# diff.relative is set, and then nothing at all for a file outside the CWD. lint's
# routing regexes are anchored (^tools/, ^kubernetes-charts/), so a lost prefix
# silently skips the file. `-C "$$ROOT"` retires all three at once.
#
# THE SAME COMMAND IS THE GUARD. `git rev-parse --show-toplevel` fails outside a
# repository AND inside a bare one — the two states where no changed set can
# honestly be computed. Getting the root and deciding whether there is one are the
# same question, so they are one call.
#
# STDOUT DECIDES; STDERR ONLY EXPLAINS. Folding both together and testing the
# result fails a perfectly good checkout on any git WARNING (`unable to access
# '/root/.gitconfig'`, which containers emit on every call). The explanation takes
# both, on the failure path only, because a bare repository answers on stdout.
#
# A REPOSITORY WITH NO COMMITS IS ASKED ABOUT DIRECTLY — `rev-parse --verify HEAD`
# rather than inferring it from a command substitution's exit status, which is also
# how a dozen other failures look. Both arms list untracked files: the fallback
# means "could not tell, so lint everything", and `git ls-files` alone lists
# TRACKED files — nothing, in the state that reaches it. --exclude-standard so
# .gitignore still applies: build output is not work.
.PHONY: lint-changed
lint-changed:
	@ROOT=$$(git rev-parse --show-toplevel 2>/dev/null); \
	if [ -z "$$ROOT" ] || [ ! -d "$$ROOT" ]; then \
		echo "lint: cannot determine what changed, so it will not claim nothing did." >&2; \
		echo "      git said: $$(git rev-parse --show-toplevel 2>&1)" >&2; \
		echo "      Run 'make LINT_ALL=1 lint' to check everything regardless." >&2; \
		exit 1; \
	fi; \
	{ if git -C "$$ROOT" rev-parse --verify -q HEAD >/dev/null; then \
		git -C "$$ROOT" diff --name-only HEAD; \
	  else \
		git -C "$$ROOT" ls-files; \
	  fi; \
	  git -C "$$ROOT" ls-files --others --exclude-standard; } | sort -u

lint:
	@set -e; \
	if [ -n "$(LINT_ALL)" ]; then \
		$(MAKE) --no-print-directory fmt-check vet staticcheck shellcheck actions-lint tf-fmt-check template-manifest-check managed-lock-check version-pins-check k8s-minor-coherence docs-guard untestable-loc-check core-surface-check $(LINT_TF) $(LINT_K8S) chart-version-guard instance-test; \
		LLZ_FUNCTIONAL_NET=0 $(MAKE) --no-print-directory llz-functional; \
		$(MAKE) --no-print-directory llz-gates; \
		exit 0; \
	fi; \
	CHANGED=$$($(MAKE) --no-print-directory lint-changed); \
	if [ -z "$$CHANGED" ]; then \
		echo "lint: nothing changed and nothing untracked (use LINT_ALL=1 to run all checks)"; \
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
	if echo "$$CHANGED" | grep -qE '^instance-template/apl-values/|^platform-apl/|^tools/internal/extensions/lifecycle/bootstrapcluster/.*\.go$$|^template-scripts/ci/scaffold-render-check\.sh$$'; then \
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
	$(MAKE) --no-print-directory llz-gates


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

# Race detector. Separate from `test`/`coverage` so a data race fails under its
# own name rather than as a confusing coverage failure, and so the ordinary
# `go test` path stays fast.
#
# LLZ_EXPECT_RACE=1 arms the canary in internal/cli/racegate_test.go: it asserts the
# binary really was built with -race. Without it, a step that silently lost the
# flag would still report green while detecting nothing — the same failure shape
# as a mutation run that reports 100% because it never spawned a test process.
# Do not drop the variable to quiet that test; it is the only thing proving this
# target does what its name says.
test-race:
	cd $(GO_DIR) && LLZ_EXPECT_RACE=1 go test -race ./...

# staticcheck. `go vet` is deliberately conservative — it reports only what is
# almost certainly a mistake. staticcheck's SA* checks cover the adjacent ground:
# impossible conditions, values assigned and never read, misused stdlib. It found
# one real gap on introduction (SA4006: a test re-stubbed its call recorder for
# the build-failure case and then never asserted it, so that case checked the
# error wrap but not that the earlier steps ran).
#
# Pinned, not @latest: a floating linter turns an unrelated PR red when the tool
# gains a check, which trains people to ignore it.
#
# ST1005 (error-string style) stays ENABLED. The eight places this codebase
# legitimately breaks it — multi-line operator diagnostics whose punctuation
# precedes an embedded newline of remediation, and one usage string whose "..."
# is variadic syntax — carry per-site //lint:ignore directives with reasons.
# Blanket-disabling a check because it flagged you is how a linter stops meaning
# anything; staticcheck errors on a directive that matches nothing, so a stale
# exception cannot rot silently either.
STATICCHECK_VERSION ?= 2025.1.1
DEADCODE_VERSION ?= latest
staticcheck:
	@cd $(GO_DIR) && if command -v staticcheck >/dev/null 2>&1; then \
	  staticcheck ./...; \
	else \
	  echo "staticcheck not on PATH — falling back to 'go run' (make install-tools installs it)"; \
	  go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...; \
	fi

# deadcode: functions unreachable from the llz entry point. REPORT ONLY — this is
# deliberately NOT a CI gate, because "unreachable from main" and "should be
# deleted" are different claims and this repo has three legitimate reasons for the
# gap:
#
#   * implemented ahead of wiring — internal/forge carries a whole GitLab client
#     behind the documented Forge abstraction (LLZ_FORGE, docs/designs/
#     forge-abstraction.md). It is a second backend awaiting selection, not rot;
#     deleting it would remove the point of the interface.
#   * test-only production helpers — reachable from tests but nothing else. Worth
#     looking at, since that usually means either a missing caller or a function
#     that should not be production code.
#   * genuinely orphaned subsystems, which is what makes this worth running: it
#     found that ALL of internal/clusterspec/aplversion.go — the apl-chart
#     major-drift gate, its LLZ_ALLOW_APL_CHART_MAJOR_DRIFT override and its
#     warnings — is called by nothing. See the tracking issue.
#
# Gating on it would force the first class to be suppressed forever, which trains
# people to ignore the third.
deadcode:
	@cd $(GO_DIR) && if command -v deadcode >/dev/null 2>&1; then \
	  deadcode ./cmd/llz; \
	else \
	  go run golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION) ./cmd/llz; \
	fi

# Fuzzing. NOT a CI gate: fuzzing is non-deterministic and open-ended, so gating
# on it would make the build flaky rather than safe. Instead the SEED CORPORA run
# as ordinary subtests on every `go test` — free, deterministic regression cover —
# and this target explores beyond them on demand.
#
# FUZZTIME defaults to 60s per target; raise it for a real hunt (FUZZTIME=10m).
# A crasher is written to the package's testdata/fuzz/ by the toolchain: COMMIT
# THAT FILE. It then replays forever as part of the seed corpus, which is how a
# one-off fuzz find becomes a permanent test.
FUZZTIME ?= 60s
fuzz:
	@set -e; cd $(GO_DIR); \
	for pkg in ./internal/cli ./internal/terraform; do \
	  for t in $$(go test $$pkg -list 'Fuzz.*' | grep '^Fuzz'); do \
	    echo "── $$pkg $$t ($(FUZZTIME))"; \
	    go test $$pkg -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME); \
	  done; \
	done

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

# coverage-bank: raise every floor to what its package now measures.
#
# THE FLOORS WERE A MANUAL RATCHET AND SLACK IS INVISIBLE. A package at 86%
# against a floor of 80 reports `ok`, and those six points are free for the next
# change to spend without anyone deciding to. One guard added in a single session
# moved its floor four times by hand, each bump after a red run.
#
# It NEVER lowers a floor and refuses to run while anything is red — see bank.go.
# Run it after adding tests, and commit the Makefile change with them.
coverage-bank:
	@cd $(GO_DIR) && go run ./cmd/llz ci check-coverage --bank \
		--profile "$(CURDIR)/coverage/tools.out" --makefile "$(CURDIR)/Makefile" $(COVERAGE_MINS)

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

# upgrade-test: the DAY-2 half of instance-test. That target proves `copier copy`
# (scaffold) works; this one proves the UPGRADE does — scaffold at each of the last
# three releases, run the real `llz upgrade` to HEAD, and assert each one ran
# unattended, kept every answer it does not own, moved the pin, left no merge
# artifacts, and produced an instance IDENTICAL to a fresh scaffold at HEAD.
# The upgrade path appeared in no workflow, target or script before this, so what
# every adopter does on day 2 was gated by nobody. Offline and cloud-free; skips
# itself when copier is absent or the clone has no tags.
#
# THREE RELEASES, not one: the instance that breaks is the one that SKIPPED
# releases, and a single hop only ever covered the adopter who upgrades weekly.
# Costs one scaffold + one upgrade per release (~2.5 min for all three, measured);
# `llz ci upgrade-test --depth 1` while iterating, but do not pin a depth in CI —
# the gate's default is what keeps `make upgrade-test` and the CI step honest
# about testing the same thing.
#
# LLZ_FORCE_SOURCE: the gate runs the tree's own `llz upgrade` — including the
# manifest-policy and removals passes, which are Go in this repo — so it has to be
# the binary built from the tree under test. Same reason docs-guard sets it.
upgrade-test: export LLZ_FORCE_SOURCE := 1
upgrade-test:
	$(call LLZ_CI,upgrade-test,--template ..)

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
