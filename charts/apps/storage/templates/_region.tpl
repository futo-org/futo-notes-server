{{- /* Everything that differs between regions derives from .Values.region, so
a region's release only sets that one value. Idempotent: every template calls
it before rendering, since Helm gives no ordering between template files. */}}
{{- define "storage.region" -}}
{{- $_ := set .Values "fullnameOverride" (.Values.fullnameOverride | default (printf "notes-storage-%s" .Values.region)) }}
{{- $_ := set .Values "s3Bucket" (.Values.s3Bucket | default (printf "notes-%s" .Values.region)) }}
{{- end -}}
