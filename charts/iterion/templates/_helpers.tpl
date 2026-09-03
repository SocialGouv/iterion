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

.Values.image.digest wins over the tag when set. A MOVING tag (`edge`,
`main`, `latest`) is resolved per POD, at that pod's own start time — so a
publish landing mid-rollout splits one ReplicaSet across two builds, and a
later HPA/KEDA scale-up from an existing ReplicaSet silently pulls whatever
the tag points at then, with no rollout-history entry. Measured on prod
2026-09-02: three server pods of ReplicaSet 548c4b7f7d serving v3.92.0 and
v3.93.0 at once. A digest is the only reference that makes "one ReplicaSet =
one build" true, which is what the per-PodTemplate rollout epoch assumes one
layer up.
*/}}
A malformed digest must abort the render, not reach the cluster: a bad shape
still concatenates into a syntactically fine string that helm lints happily
and only kubelet rejects (`InvalidImageName`), so no pod ever starts — behind
`maxUnavailable: 0` the rollout then stalls in silence while the old pods keep
serving, and a KEDA scale-up simply never gets capacity. This chart has
already been bitten by the shape once, on the neighbouring field: a digest
written into `tag:` rendered `…iterion:@sha256:…` (see
docs/bot-runs/dep-update-guard.md). Same reasoning as
`iterion.nats.monitoringEndpoint`'s `required` below.
*/}}
{{- define "iterion.image" -}}
{{- if .Values.image.digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" .Values.image.digest) -}}
{{- fail (printf "image.digest must be sha256:<64 hex chars>, got %q — pass the digest alone, not a tag or a full image reference" .Values.image.digest) -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
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
  3. Fail-fast via `required` when KEDA is enabled but no endpoint
     can be resolved — silently rendering an empty string used to
     leave the ScaledObject unable to scrape lag, which KEDA accepts
     without surfacing an error and runners stayed pinned at
     minReplicas. Caller must either enable the bundled NATS sub-chart
     or set config.nats.monitoringEndpoint explicitly.
*/}}
{{- define "iterion.nats.monitoringEndpoint" -}}
{{- if .Values.config.nats.monitoringEndpoint -}}
{{- .Values.config.nats.monitoringEndpoint -}}
{{- else if .Values.nats.enabled -}}
{{- printf "%s-nats:8222" .Release.Name -}}
{{- else if .Values.runner.keda.enabled -}}
{{- required "runner.keda.enabled=true requires either nats.enabled=true or config.nats.monitoringEndpoint to be set so the JetStream lag scaler can scrape the monitoring port" "" -}}
{{- end -}}
{{- end -}}
