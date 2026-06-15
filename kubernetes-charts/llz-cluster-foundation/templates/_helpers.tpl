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
