{{/*
Expand the name of the chart.
*/}}
{{- define "vm-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "vm-manager.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label value.
*/}}
{{- define "vm-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "vm-manager.labels" -}}
helm.sh/chart: {{ include "vm-manager.chart" . }}
{{ include "vm-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "vm-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vm-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "vm-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "vm-manager.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The state directory (--state-dir) and the image directory (--image-dir) inside
the container. Short on purpose: unix socket paths live below the state dir.
*/}}
{{- define "vm-manager.stateDir" -}}/var/lib/vm-manager{{- end }}
{{- define "vm-manager.imageDir" -}}/var/lib/vm-manager/images{{- end }}

{{/*
The state claim's name: the existing one, else the chart's own when created.
Empty means an emptyDir.
*/}}
{{- define "vm-manager.stateClaim" -}}
{{- if .Values.persistence.existingClaim }}{{ .Values.persistence.existingClaim }}{{ else if .Values.persistence.create }}{{ include "vm-manager.fullname" . }}-state{{ end }}
{{- end }}

{{/*
The platform identity contract (global.identity), an empty dict when absent.
*/}}
{{- define "vm-manager.globalIdentity" -}}
{{- dig "identity" (dict) (.Values.global | default dict) | toJson }}
{{- end }}

{{/*
Existing Secret with the provider credentials: oauth.existingSecret, else the
platform's global.identity.existingSecret, else the chart-rendered one.
*/}}
{{- define "vm-manager.oauthSecretName" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "vm-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret (no existing one named).
*/}}
{{- define "vm-manager.oauthRendersSecret" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- if and .Values.oauth.enabled (not .Values.oauth.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
OAuth base URL: oauth.baseURL, else https://<fullname>.<global.domain>.
*/}}
{{- define "vm-manager.oauthBaseURL" -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "vm-manager.fullname" .) $domain }}{{ end -}}
{{- required "oauth.baseURL is required when oauth.enabled (or set global.domain)" (.Values.oauth.baseURL | default $derived) }}
{{- end }}

{{/*
Dex issuer / client id with the global.identity fallbacks.
*/}}
{{- define "vm-manager.oauthDexIssuerURL" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.issuerURL (or global.identity.issuerUrl) is required for the dex provider" (.Values.oauth.dex.issuerURL | default (dig "issuerUrl" "" $g)) }}
{{- end }}

{{- define "vm-manager.oauthDexClientID" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.clientID (or global.identity.clientId) is required for the dex provider" (.Values.oauth.dex.clientID | default (dig "clientId" "" $g)) }}
{{- end }}

{{/*
CA Secret of a private-certificate Dex: oauth.dex.caSecret, else
global.identity.ca. Name empty means system trust.
*/}}
{{- define "vm-manager.oauthDexCASecretName" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.dex.caSecret.name | default (dig "ca" "secretName" "" $g) }}
{{- end }}

{{- define "vm-manager.oauthDexCASecretKey" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- if .Values.oauth.dex.caSecret.name }}{{ .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ else }}{{ dig "ca" "key" "" $g | default .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ end }}
{{- end }}

{{/*
Trusted audiences, comma-separated: the OAuth client ids whose IdP id_tokens
this server accepts as bearer tokens. The union, in this order and without
duplicates, of oauth.trustedAudiences (else the platform client,
global.identity.clientId) and muster.mcpServer.auth.requiredAudiences — every
token muster forwards carries the latter by construction.
*/}}
{{- define "vm-manager.oauthTrustedAudiences" -}}
{{- $g := include "vm-manager.globalIdentity" . | fromJson -}}
{{- $base := .Values.oauth.trustedAudiences | default (list (dig "clientId" "" $g)) -}}
{{- $auds := list -}}
{{- range concat $base (.Values.muster.mcpServer.auth.requiredAudiences | default list) -}}
{{- if and . (not (has . $auds)) }}{{ $auds = append $auds . }}{{ end -}}
{{- end -}}
{{- join "," $auds -}}
{{- end }}
