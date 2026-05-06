{{/*
Expand the chart name. Used by every label/selector. Long releases
collapse to 63 chars (the K8s label limit).
*/}}
{{- define "iterion.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. Combines release + chart for uniqueness
across multiple installs in the same namespace.
*/}}
{{- define "iterion.fullname" -}}
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
{{- end -}}

{{/*
Common labels — applied to every resource. selectorLabels (a subset)
are immutable across rolling updates and must match across
Service/Deployment.
*/}}
{{- define "iterion.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "iterion.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "iterion.selectorLabels" -}}
app.kubernetes.io/name: {{ include "iterion.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Container image reference. Defaults to .Chart.AppVersion when
.Values.image.tag is empty so a fresh chart pull tracks the binary
release out of the box.
*/}}
{{- define "iterion.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
ServiceAccount name. Use the override when set, otherwise derive
from the release.
*/}}
{{- define "iterion.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "iterion.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
NATS monitoring endpoint for the KEDA scaler. The scaler scrapes the
HTTP `/jsz` endpoint, which lives on a separate port from the client
protocol. Resolution order:
  1. Explicit override via .Values.config.nats.monitoringEndpoint
  2. Bundled nats sub-chart (default port 8222 on `<release>-nats`)
  3. Empty string — caller must set the value or the ScaledObject
     will fail to scrape and KEDA will hold replicas at minReplicas.
*/}}
{{- define "iterion.nats.monitoringEndpoint" -}}
{{- if .Values.config.nats.monitoringEndpoint -}}
{{- .Values.config.nats.monitoringEndpoint -}}
{{- else if .Values.nats.enabled -}}
{{- printf "%s-nats:8222" .Release.Name -}}
{{- end -}}
{{- end -}}
