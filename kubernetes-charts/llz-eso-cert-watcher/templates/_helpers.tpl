{{/*
Expand the name of the chart.
*/}}
{{- define "llz-eso-cert-watcher.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Honors fullnameOverride, else nameOverride/chart name.
*/}}
{{- define "llz-eso-cert-watcher.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- include "llz-eso-cert-watcher.name" . | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "llz-eso-cert-watcher.labels" -}}
app.kubernetes.io/name: {{ include "llz-eso-cert-watcher.name" . }}
app.kubernetes.io/component: cert-rotation-trigger
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels — the stable subset used for the Deployment selector. Must NOT
include version/chart labels (immutable selector). This is exactly the label the
restart machinery and any NetworkPolicy must match.
*/}}
{{- define "llz-eso-cert-watcher.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llz-eso-cert-watcher.name" . }}
{{- end }}

{{/*
Target ESO Deployment namespace — defaults to the watcher namespace.
*/}}
{{- define "llz-eso-cert-watcher.targetNamespace" -}}
{{- default .Values.namespace .Values.target.deployment.namespace }}
{{- end }}
