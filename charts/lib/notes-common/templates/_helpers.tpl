{{/*
Fully qualified app name. Falls back to .Release.Name-.Chart.Name.
*/}}
{{- define "notes-common.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "notes-common.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "notes-common.labels" -}}
app.kubernetes.io/name: {{ include "notes-common.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "notes-common.selectorLabels" -}}
app.kubernetes.io/name: {{ include "notes-common.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
