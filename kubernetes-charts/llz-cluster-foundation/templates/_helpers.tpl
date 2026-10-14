{{/*
Expand the name of the chart.
*/}}
{{- define "llz-cluster-foundation.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Honors fullnameOverride, else nameOverride/chart name.
NOTE: this chart deliberately does NOT prefix the cluster-singleton resources it
manages (the `coredns-custom` ConfigMap, the `workload-coredns`/storageclass
patcher Jobs, the per-namespace NetworkPolicies). Those names are contracts that
LKE's base Corefile, apl-core, and other cluster components reference literally —
renaming them would silently break the import/selector wiring. fullname is used
only for chart-level labels.
*/}}
{{- define "llz-cluster-foundation.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- include "llz-cluster-foundation.name" . | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "llz-cluster-foundation.labels" -}}
app.kubernetes.io/name: {{ include "llz-cluster-foundation.name" . }}
app.kubernetes.io/component: llz-cluster-foundation
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
scPatcher.podTemplate — the shared PodTemplateSpec (demote linode-block-storage-retain
+ verify-one-default script) used by BOTH the PostSync Job and the durable CronJob in
sc-default-patcher-job.yaml, so the logic lives in exactly one place. Emitted at column
0; callers `nindent` it under their `template:`. Takes the root context.
*/}}
{{- define "scPatcher.podTemplate" -}}
{{- $sc := .Values.storageClassPatcher -}}
metadata:
  labels:
    app.kubernetes.io/name: sc-default-patcher
spec:
  serviceAccountName: sc-default-patcher
  restartPolicy: OnFailure
  containers:
    - name: patch
      image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      command: [sh, -c]
      args:
        - |
          set -euo pipefail
          echo "Verifying {{ $sc.desiredDefault }} exists..."
          if ! kubectl get sc {{ $sc.desiredDefault }} >/dev/null 2>&1; then
            # The SC is rendered before this runs, so this is a "the SC didn't
            # actually install" symptom — fail loud so `llz ci converge` sees it.
            echo "::error::{{ $sc.desiredDefault }} not found. Not demoting {{ $sc.demote }} — would leave the cluster with no default StorageClass." >&2
            exit 1
          fi

          echo "=== Demote {{ $sc.demote }} from default ==="
          if kubectl get sc {{ $sc.demote }} >/dev/null 2>&1; then
            kubectl annotate sc {{ $sc.demote }} \
              storageclass.kubernetes.io/is-default-class=false --overwrite
          else
            echo "{{ $sc.demote }} not found on cluster — nothing to demote (single-class cluster?)."
          fi

          # Verify exactly one default remains, and it's ours.
          DEFAULTS=$(kubectl get sc -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}' \
            | awk -F'\t' '$2=="true"{print $1}')
          echo "Default StorageClasses after patch:"
          echo "$DEFAULTS"
          if [ "$DEFAULTS" != "{{ $sc.desiredDefault }}" ]; then
            echo "::error::expected exactly '{{ $sc.desiredDefault }}' as default; got: $DEFAULTS" >&2
            exit 1
          fi
          echo "Done."
      resources:
        {{- toYaml $sc.resources | nindent 8 }}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
        readOnlyRootFilesystem: true
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile:
          type: RuntimeDefault
{{- end -}}

{{/*
baselineNP renders the two NetworkPolicies every apl-core-managed namespace gets
first: a default-deny (Ingress+Egress) and the allow-dns egress that must
accompany it — a default-deny with no DNS allowance breaks every pod's name
resolution, which is why these two are emitted as one unit and never separately.

The four namespaces (cert-manager, harbor, istio-system, observability) had
byte-identical copies of both, differing only in the name prefix and namespace.
Called inline at each namespace's own section rather than hoisted into a range
at the top of the file, so the per-namespace grouping the file documents
("Pattern per namespace: default-deny + allow-dns + allow-apiserver +
namespace-specific allows") is preserved and the rendered document order is
unchanged.

Args (dict): prefix (name prefix), ns (namespace), wave (sync-wave),
dnsPorts (the .Values.networkPolicies.dnsPorts list).
*/}}
{{- define "llz-cluster-foundation.baselineNP" -}}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ .prefix }}-default-deny
  namespace: {{ .ns }}
  annotations:
    argocd.argoproj.io/sync-wave: {{ .wave | quote }}
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ .prefix }}-allow-dns
  namespace: {{ .ns }}
  annotations:
    argocd.argoproj.io/sync-wave: {{ .wave | quote }}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - ports:
        {{- range .dnsPorts }}
        - { port: {{ .port }}, protocol: {{ .protocol }} }
        {{- end }}
{{- end }}
