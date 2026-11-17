{{- /* The regions file the API reads (REGIONS_FILE), rendered from
.Values.regions into the regions ConfigMap. */}}
{{- define "api.regionsJson" -}}
{{- .Values.regions | toPrettyJson }}
{{- end -}}

{{- define "api.regionsConfigMap" -}}
{{- printf "%s-regions" (include "notes-common.fullname" .) }}
{{- end -}}

{{- /* POSTGRES_* from the "<cluster>-app" Secret CloudNativePG generates. */}}
{{- define "api.postgresEnv" -}}
{{- $secret := printf "%s-app" .Values.postgresClusterName }}
{{- $env := list }}
{{- range $name, $key := dict "POSTGRES_HOST" "host" "POSTGRES_PORT" "port" "POSTGRES_DATABASE" "dbname" "POSTGRES_USERNAME" "username" "POSTGRES_PASSWORD" "password" }}
{{- $env = append $env (dict "name" $name "valueFrom" (dict "secretKeyRef" (dict "name" $secret "key" $key))) }}
{{- end }}
{{- toYaml $env }}
{{- end -}}
