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

{{- /* APPLE_* and PLAY_* for the store billing providers, each only when its
store is on (appStore.bundleId, play.packageName). Their keys come from the
Secret. */}}
{{- define "api.storeEnv" -}}
{{- $env := list }}
{{- with .Values.appStore }}
{{- if .bundleId }}
{{- $env = concat $env (list
  (dict "name" "APPLE_BUNDLE_ID" "value" .bundleId)
  (dict "name" "APPLE_APP_APPLE_ID" "value" (.appAppleId | int64 | toString))
  (dict "name" "APPLE_ISSUER_ID" "value" .issuerId)
  (dict "name" "APPLE_KEY_ID" "value" .keyId)
  (dict "name" "APPLE_SUBSCRIPTION_PRODUCT_IDS" "value" (join "," .subscriptionProductIds))
  (dict "name" "APPLE_ACCEPT_SANDBOX" "value" (toString .acceptSandbox))
) }}
{{- end }}
{{- end }}
{{- with .Values.play }}
{{- if .packageName }}
{{- $env = concat $env (list
  (dict "name" "PLAY_PACKAGE_NAME" "value" .packageName)
  (dict "name" "PLAY_SUBSCRIPTION_PRODUCT_IDS" "value" (join "," .subscriptionProductIds))
  (dict "name" "PLAY_RTDN_AUDIENCE" "value" .rtdnAudience)
  (dict "name" "PLAY_RTDN_SERVICE_ACCOUNT" "value" .rtdnServiceAccount)
) }}
{{- end }}
{{- end }}
{{- toYaml $env }}
{{- end -}}
