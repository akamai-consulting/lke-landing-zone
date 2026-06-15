{{/*
Expand the name of the chart.
*/}}
{{- define "llz-cert-automation.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Honors fullnameOverride, else nameOverride/chart name.
*/}}
{{- define "llz-cert-automation.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- include "llz-cert-automation.name" . | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "llz-cert-automation.labels" -}}
app.kubernetes.io/name: {{ include "llz-cert-automation.name" . }}
app.kubernetes.io/component: cert-renewal
app.kubernetes.io/part-of: lke-landing-zone
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
ServiceAccount used by the Sensor and the Argo Workflow pods it spawns.
*/}}
{{- define "llz-cert-automation.runnerServiceAccount" -}}
{{- printf "%s-runner" (include "llz-cert-automation.name" .) }}
{{- end }}
