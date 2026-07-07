#!/usr/bin/env bash
# vap-admission-check.sh — prove the llz-wave-health-guard ValidatingAdmissionPolicy
# COMPILES and ENFORCES correctly, on a real apiserver (the lint.yml kind cluster).
#
# Why this exists beyond the other gates:
#   - kubeconform validates the VAP's SCHEMA, not its CEL.
#   - TestWaveHealthVAPMatchesGuard pins the allowlist SET to the Go guard, not CEL
#     compilation or runtime behavior.
# A CEL syntax error, or a matchConditions/exclusion mistake, passes both and fails
# only when a cluster registers the policy — or, worse, wedges real admissions. The
# kind apiserver COMPILES the CEL on apply (so a bad expression fails this step), and
# a label-scoped Deny binding lets us assert enforcement with zero blast radius.
#
# Regression-locks the two review findings that a live e2e census surfaced:
#   - an unvetted health-checked kind at a negative wave IS denied;
#   - argoproj.io CRs at negative waves are NOT denied (Sensor/EventSource/EventBus/
#     Workflow are child-App-managed — the group is excluded).
#
# Requires kubectl + a cluster with the argoproj.io CRDs installed (the dry-run job
# installs Argo Events). Usage: vap-admission-check.sh [repo-root]
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
POLICY="$ROOT/instance-template/apl-values/_shared/manifest/admission/wave-health-policy.yaml"
FAILED=0

cleanup() {
  kubectl delete validatingadmissionpolicybinding llz-wave-health-guard-citest --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete validatingadmissionpolicy llz-wave-health-guard --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Applying the policy COMPILES its CEL — a syntax error fails here.
echo "Applying llz-wave-health-guard (compiles the CEL)…"
kubectl apply -f "$POLICY"

# Deny only objects carrying the test label — zero blast radius.
kubectl apply -f - <<'YAML'
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: llz-wave-health-guard-citest
spec:
  policyName: llz-wave-health-guard
  validationActions: [Deny]
  matchResources:
    objectSelector:
      matchLabels:
        llz-vap-test: "true"
YAML

# The binding does NOT enforce the instant `kubectl apply` returns — the apiserver
# compiles the CEL and propagates the binding to the admission plane, and that lag is
# variable under CI load. A fixed sleep raced it: if enforcement wasn't live yet, the
# first deny-probe was ADMITTED → false failure on a correct policy. Poll for actual
# enforcement (a known-bad object is DENIED *by our policy*) instead, bounded so a
# genuinely broken/non-enforcing policy still fails the step.
read -r -d '' READY_PROBE <<'YAML' || true
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vaptest-ready
  namespace: default
  labels: { llz-vap-test: "true" }
  annotations: { argocd.argoproj.io/sync-wave: "-5" }
spec:
  selector: { matchLabels: { app: vaptest } }
  template:
    metadata: { labels: { app: vaptest } }
    spec:
      containers: [{ name: c, image: registry.k8s.io/pause:3.9 }]
YAML
echo "Waiting for the binding to begin enforcing…"
enforcing=0
for _ in $(seq 1 30); do   # ~60s cap (30 × 2s)
  out="$(printf '%s' "$READY_PROBE" | kubectl apply --dry-run=server -f - 2>&1)" && rc=0 || rc=1
  if [[ $rc -ne 0 && "$out" == *"llz-wave-health-guard"* ]]; then enforcing=1; break; fi
  sleep 2
done
[[ "$enforcing" -eq 1 ]] || { echo "::error::llz-wave-health-guard binding did not begin enforcing within 60s — CEL/binding may be broken"; exit 1; }

# probe <name> <expect: deny|allow> <<manifest — dry-run apply and assert the outcome.
probe() {
  local name="$1" expect="$2" out rc
  out="$(kubectl apply --dry-run=server -f - 2>&1)" && rc=0 || rc=1
  if [[ "$expect" == "deny" ]]; then
    if [[ $rc -ne 0 && "$out" == *"llz-wave-health-guard"* ]]; then
      echo "ok  : $name → DENIED by the policy"
    else
      echo "::error::$name should have been DENIED but was not (rc=$rc): $out"; FAILED=1
    fi
  else
    if [[ $rc -eq 0 ]]; then
      echo "ok  : $name → admitted"
    else
      echo "::error::$name should have been ADMITTED but was denied: $out"; FAILED=1
    fi
  fi
}

# A) unvetted health-checked kind at a NEGATIVE wave → DENY.
probe "Deployment @ wave -5 (unvetted)" deny <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vaptest-bad
  namespace: default
  labels: { llz-vap-test: "true" }
  annotations: { argocd.argoproj.io/sync-wave: "-5" }
spec:
  selector: { matchLabels: { app: vaptest } }
  template:
    metadata: { labels: { app: vaptest } }
    spec:
      containers:
        - name: c
          image: registry.k8s.io/pause:3.9
YAML

# B) allowlisted kind at a NEGATIVE wave → ADMIT.
probe "NetworkPolicy @ wave -5 (allowlisted)" allow <<'YAML'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: vaptest-np
  namespace: default
  labels: { llz-vap-test: "true" }
  annotations: { argocd.argoproj.io/sync-wave: "-5" }
spec:
  podSelector: {}
  policyTypes: [Ingress]
YAML

# C) argoproj.io kind at a NEGATIVE wave → ADMIT (group excluded — the review fix).
probe "EventBus @ wave -14 (argoproj.io excluded)" allow <<'YAML'
apiVersion: argoproj.io/v1alpha1
kind: EventBus
metadata:
  name: vaptest-eb
  namespace: default
  labels: { llz-vap-test: "true" }
  annotations: { argocd.argoproj.io/sync-wave: "-14" }
spec:
  nats:
    native: {}
YAML

# D) the same unvetted kind at a NON-negative wave → ADMIT (only negative waves gate).
probe "Deployment @ wave 5 (non-negative)" allow <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vaptest-ok
  namespace: default
  labels: { llz-vap-test: "true" }
  annotations: { argocd.argoproj.io/sync-wave: "5" }
spec:
  selector: { matchLabels: { app: vaptest2 } }
  template:
    metadata: { labels: { app: vaptest2 } }
    spec:
      containers:
        - name: c
          image: registry.k8s.io/pause:3.9
YAML

echo
if [[ "$FAILED" -ne 0 ]]; then
  echo "::error::vap-admission-check FAILED"
  exit 1
fi
echo "vap-admission-check OK"
