{{/*
Expand the name of the chart.
*/}}
{{- define "llz-argo-bootstrap-apps.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels stamped on every generated Argo CD CR. Kept off the Application
NAME (no prefix) but useful on labels for provenance / `kubectl get -l`.
*/}}
{{- define "llz-argo-bootstrap-apps.labels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: llz-argo-bootstrap-apps
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resolve the git repo URL. This is the single org-identity value a sibling team
MUST override. It renders even when left as the placeholder (so the chart
templates cleanly with defaults), but `argo-bootstrap-apps.gitRepoURL.isPlaceholder`
gates a loud warning comment next to every resource that consumed the
placeholder so it can't ship unnoticed.
*/}}
{{- define "llz-argo-bootstrap-apps.gitRepoURL" -}}
{{- required "global.gitRepoURL is required" .Values.global.gitRepoURL -}}
{{- end }}

{{/*
True when gitRepoURL is still the unconfigured placeholder.
*/}}
{{- define "llz-argo-bootstrap-apps.gitRepoURL.isPlaceholder" -}}
{{- eq (.Values.global.gitRepoURL | toString) "REPLACE_ME-git-repo-url" -}}
{{- end }}

{{/*
Render a component's spec.source block. Handles both `oci` and `git` source
types. Argument: a dict { "ctx": $, "component": <component> }.
*/}}
{{- define "llz-argo-bootstrap-apps.source" -}}
{{- $ctx := .ctx -}}
{{- $c := .component -}}
{{- $src := $c.source -}}
{{- $type := $src.type | default "oci" -}}
{{- if eq $type "oci" }}
{{- $repoURL := $src.repoURL | default $ctx.Values.global.chartsRegistry }}
repoURL: {{ $repoURL | quote }}
chart: {{ required (printf "component %q: source.chart is required for type oci" $c.name) $src.chart | quote }}
targetRevision: {{ required (printf "component %q: source.version is required for type oci" $c.name) $src.version | quote }}
helm:
  releaseName: {{ $src.releaseName | default $c.name | quote }}
  {{- with $src.valuesObject }}
  valuesObject:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- else if eq $type "git" }}
{{- $repoURL := $src.repoURL | default (include "llz-argo-bootstrap-apps.gitRepoURL" $ctx) }}
{{- if and (not $src.repoURL) (eq (include "llz-argo-bootstrap-apps.gitRepoURL.isPlaceholder" $ctx) "true") }}
# ⚠️  global.gitRepoURL is still the placeholder — set it to YOUR platform repo.
{{- end }}
repoURL: {{ $repoURL | quote }}
targetRevision: {{ $src.targetRevision | default $ctx.Values.global.targetRevision | quote }}
path: {{ required (printf "component %q: source.path is required for type git" $c.name) $src.path | quote }}
{{- with $src.helm }}
helm:
  {{- toYaml . | nindent 4 }}
{{- end }}
{{- else }}
{{- fail (printf "component %q: source.type must be 'oci' or 'git', got %q" $c.name $type) }}
{{- end }}
{{- end }}

{{/*
Render a component's syncPolicy: defaultSyncPolicy deep-merged with any
per-component override. Argument: a dict { "ctx": $, "component": <component> }.
mergeOverwrite mutates its first arg, so deep-copy the defaults first.
*/}}
{{- define "llz-argo-bootstrap-apps.syncPolicy" -}}
{{- $ctx := .ctx -}}
{{- $c := .component -}}
{{- $defaults := deepCopy $ctx.Values.defaultSyncPolicy -}}
{{- $policy := $defaults -}}
{{- if $c.syncPolicy -}}
{{- $policy = mergeOverwrite $defaults $c.syncPolicy -}}
{{- end -}}
{{- toYaml $policy -}}
{{- end }}
