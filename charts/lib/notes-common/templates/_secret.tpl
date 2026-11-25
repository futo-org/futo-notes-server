{{- define "notes-common.secret" -}}
{{- if .Values.secretData -}}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "notes-common.fullname" . }}
  labels:
    {{- include "notes-common.labels" . | nindent 4 }}
type: Opaque
stringData:
  {{- toYaml .Values.secretData | nindent 2 }}
{{- end -}}
{{- end -}}
