{{- define "kai-resource-isolator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "kai-resource-isolator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "kai-resource-isolator.namespace" -}}
{{- if .Values.namespaceOverride -}}
{{- .Values.namespaceOverride -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end }}

{{- define "kai-resource-isolator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" -}}
{{- end }}

{{- define "kai-resource-isolator.labels" -}}
helm.sh/chart: {{ include "kai-resource-isolator.chart" . }}
{{ include "kai-resource-isolator.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kai-resource-isolator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kai-resource-isolator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kai-resource-isolator.webhook.fullname" -}}
{{- printf "%s-webhook" (include "kai-resource-isolator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "kai-resource-isolator.webhook.tlsSecret" -}}
{{- printf "%s-webhook-tls" (include "kai-resource-isolator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "kai-resource-isolator.webhook.configName" -}}
{{- printf "%s-mutating" (include "kai-resource-isolator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "kai-resource-isolator.librarySync.fullname" -}}
{{- printf "%s-libsync" (include "kai-resource-isolator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "kai-resource-isolator.renderImage" -}}
{{- $reg := .registry -}}
{{- $repo := .repository -}}
{{- $tag := .tag -}}
{{- if $reg -}}
{{- printf "%s/%s:%s" $reg $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{- define "kai-resource-isolator.image" -}}
{{- $img := .Values.image -}}
{{- $global := .Values.global.imageRegistry -}}
{{- $reg := default $global $img.registry -}}
{{- include "kai-resource-isolator.renderImage" (dict "registry" $reg "repository" $img.repository "tag" $img.tag) -}}
{{- end }}

{{- define "kai-resource-isolator.patch.imageLegacy" -}}
{{- $img := .Values.tls.patch.image -}}
{{- $global := .Values.global.imageRegistry -}}
{{- $reg := default $global $img.registry -}}
{{- include "kai-resource-isolator.renderImage" (dict "registry" $reg "repository" $img.repository "tag" $img.tag) -}}
{{- end }}

{{- define "kai-resource-isolator.patch.imageNew" -}}
{{- $img := .Values.tls.patch.imageNew -}}
{{- $global := .Values.global.imageRegistry -}}
{{- $reg := default $global $img.registry -}}
{{- include "kai-resource-isolator.renderImage" (dict "registry" $reg "repository" $img.repository "tag" $img.tag) -}}
{{- end }}

{{- define "kai-resource-isolator.pullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}
