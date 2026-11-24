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
The digest guard behind `image.requireDigest` (opt-in, default false): the
ONE place every image reference a pod runs passes through, so a production
install can refuse a moving tag at render time instead of discovering it as
a ReplicaSet running two builds. Takes a dict: `root` (the top context),
`field` (the values path the operator fixes) and `ref` (the reference
rendered). Returns the reference unchanged when the guard is off or the
reference is `repo@sha256:<64 hex>`; a `fail` otherwise. The shape is
anchored at both ends: a bare `@sha256:…` (no repository) is exactly the
string kubelet alone rejects as InvalidImageName, so it must not pass the
render either. Helm has no warning channel, so opt-in is the escape hatch:
nothing changes for an install that leaves the flag false.
*/}}
{{- define "iterion.pinnedImage" -}}
{{- if and .root.Values.image.requireDigest (not (regexMatch "^[^@\\s]+@sha256:[0-9a-f]{64}$" .ref)) -}}
{{- fail (printf "image.requireDigest=true: %s resolves to %q, which is not a digest reference (repo@sha256:<64 hex chars>) — pin it by digest (docs/cloud-deployment.md § Pinning the image) or set image.requireDigest=false" .field .ref) -}}
{{- end -}}
{{- .ref -}}
{{- end -}}

{{/*
The server image: the shared reference, under the digest guard.
*/}}
{{- define "iterion.serverImage" -}}
{{- include "iterion.pinnedImage" (dict "root" . "field" "image (the server image)" "ref" (include "iterion.image" .)) -}}
{{- end -}}

{{/*
The shared runner image: `runner.image` verbatim when set (a full
reference, e.g. the devbox runner), the shared reference otherwise; under
the digest guard either way, because `image.digest` pins the SHARED
reference only and a runner given its own image keeps the hazard.
*/}}
{{- define "iterion.runnerImage" -}}
{{- if .Values.runner.image -}}
{{- include "iterion.pinnedImage" (dict "root" . "field" "runner.image" "ref" .Values.runner.image) -}}
{{- else -}}
{{- include "iterion.pinnedImage" (dict "root" . "field" "image (the runner image)" "ref" (include "iterion.image" .)) -}}
{{- end -}}
{{- end -}}

{{/*
A pool's runner image: the pool's own `image` verbatim when set, the shared
reference otherwise; under the digest guard either way. Takes a dict:
`root` (the top context) and `pool` (the runnerPools[] entry).
*/}}
{{- define "iterion.runnerPoolImage" -}}
{{- if .pool.image -}}
{{- include "iterion.pinnedImage" (dict "root" .root "field" (printf "runnerPools[%s].image" .pool.name) "ref" .pool.image) -}}
{{- else -}}
{{- include "iterion.pinnedImage" (dict "root" .root "field" (printf "image (the image of pool %s)" .pool.name) "ref" (include "iterion.image" .root)) -}}
{{- end -}}
{{- end -}}

{{/*
Whether a pool resolves the kubernetes sandbox (ADR-162 Q3): the pool's own
`sandbox.enabled` when it spells the key (hasKey, not sprig default: default
reads false as absent, and a pool could never opt out of a release-wide
true), runner.sandbox.enabled otherwise. Renders "true" or nothing, so a
caller reads it with `if`. A value that is not a boolean is refused: the
string "false" is truthy to the templates. So is a `sandbox` that is not a
map — even a falsy one, which sprig's default would read as absent — and a
key of it other than `enabled`: a misspelled flag (`enable`, `Enabled`)
would otherwise leave the pool following the release with no refusal, on
the one flag that decides whether its runs are sandboxed. Takes a dict:
`root` (the top context) and `pool` (the runnerPools[] entry).
*/}}
{{- define "iterion.poolSandbox" -}}
{{- $own := dict -}}
{{- if and (hasKey .pool "sandbox") (not (kindIs "invalid" .pool.sandbox)) -}}
{{- $own = .pool.sandbox -}}
{{- end -}}
{{- if not (kindIs "map" $own) -}}
{{- fail (printf "runnerPools[%s].sandbox must be a map (sandbox.enabled: true|false), got %v" .pool.name $own) -}}
{{- end -}}
{{- range $key, $_ := $own -}}
{{- if ne $key "enabled" -}}
{{- fail (printf "runnerPools[%s].sandbox.%s is not a key of a pool's sandbox — the only one is sandbox.enabled (true|false), and a misspelled flag would leave the pool following the release with no refusal" $.pool.name $key) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $own "enabled" -}}
{{- if not (kindIs "bool" $own.enabled) -}}
{{- fail (printf "runnerPools[%s].sandbox.enabled must be a boolean, got %v (%s) — the string \"false\" is truthy to the templates" .pool.name $own.enabled (kindOf $own.enabled)) -}}
{{- end -}}
{{- if $own.enabled -}}true{{- end -}}
{{- else if .root.Values.runner.sandbox.enabled -}}true{{- end -}}
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

{{/*
iterion.mongo.uri — the Mongo URI the ConfigMap carries: config.mongo.uri
when the operator sets it, else the dev Mongo's Service when
devStack.mongodb is on (its one-member replica set, whatever the release
name), else empty — an install whose auth Secret carries ITERION_MONGO_URI
leaves it so (the Secret's envFrom comes after the ConfigMap's and wins).
*/}}
{{- define "iterion.mongo.uri" -}}
{{- if .Values.config.mongo.uri -}}
{{- .Values.config.mongo.uri -}}
{{- else if ((.Values.devStack).mongodb).enabled -}}
{{- printf "mongodb://%s:27017/?replicaSet=%s" (include "iterion.devStoreName" (dict "root" . "suffix" "mongodb")) ((.Values.devStack.mongodb).replicaSetName | default "rs0") -}}
{{- end -}}
{{- end -}}

{{/*
iterion.s3.endpoint — the S3 endpoint the pods and the backup CronJobs
dial: config.s3.endpoint when the operator sets it, else the dev SeaweedFS
gateway's Service when devStack.seaweedfs is on (the dev stack resolves
its own endpoint whatever the release name), else empty.
*/}}
{{- define "iterion.s3.endpoint" -}}
{{- if .Values.config.s3.endpoint -}}
{{- .Values.config.s3.endpoint -}}
{{- else if ((.Values.devStack).seaweedfs).enabled -}}
{{- printf "http://%s:8333" (include "iterion.devStoreName" (dict "root" . "suffix" "seaweedfs")) -}}
{{- end -}}
{{- end -}}

{{/*
iterion.devStoreName — the name of a dev store object: the release's
fullname cut so the suffix still fits in 63 characters. Cutting AFTER the
suffix folds the stores onto the server's own name (a 62-character
fullnameOverride then renders three Deployments and three Services under
one name, and the Mongo URI dials the server). Dict: root, suffix.
*/}}
{{- define "iterion.devStoreName" -}}
{{- printf "%s-%s" (include "iterion.fullname" .root | trunc (int (sub 62 (len .suffix))) | trimSuffix "-") .suffix -}}
{{- end -}}

{{/*
iterion.scheduling — the affinity and topologySpreadConstraints of ONE
workload, rendered from its OWN selector labels (#2012). Call with a dict:
  root        the chart root ($)
  component   the app.kubernetes.io/component value of the workload
  extraLabels (optional) labels the workload's pods carry beyond the
              component — e.g. the pool label
  scheduling  the knob block (server.scheduling, runner.scheduling or the
              pool's own, merged over the runner's)
  affinity    (optional) the operator's free-form affinity, merged OVER the
              generated one key by key: nodeAffinity and a term list of the
              OTHER kind (required vs preferred) are kept alongside, a term
              list of the SAME kind REPLACES the generated one (lists do not
              merge) — scheduling.antiAffinity: none renders this alone
The selector is never typed by hand: it is the chart's selectorLabels plus
the component (and extra) labels — the same ones the pod template carries —
so it cannot name a label the pods do not have. That dead-selector class is
what `task chart:check` (internal/rendergate) refuses, after ovh-prod's
valkeyha spread its three replicas by luck alone (#2012).
*/}}
{{- define "iterion.scheduling" -}}
{{- $root := .root -}}
{{- $component := .component -}}
{{- $s := .scheduling | default dict -}}
{{- $preset := $s.antiAffinity | default "" -}}
{{- if not (has $preset (list "" "none" "soft" "hard")) -}}
{{- fail (printf "scheduling.antiAffinity must be soft, hard or none (got %q) on the %s workload" $preset $component) -}}
{{- end -}}
{{- $key := $s.topologyKey | default "kubernetes.io/hostname" -}}
{{- $labels := dict "app.kubernetes.io/name" (include "iterion.name" $root) "app.kubernetes.io/instance" $root.Release.Name "app.kubernetes.io/component" $component -}}
{{- range $k, $v := (.extraLabels | default dict) -}}
{{- $_ := set $labels $k $v -}}
{{- end -}}
{{- $term := dict "topologyKey" $key "labelSelector" (dict "matchLabels" $labels) -}}
{{- $generated := dict -}}
{{- if eq $preset "hard" -}}
{{- $_ := set $generated "podAntiAffinity" (dict "requiredDuringSchedulingIgnoredDuringExecution" (list $term)) -}}
{{- else if eq $preset "soft" -}}
{{- $_ := set $generated "podAntiAffinity" (dict "preferredDuringSchedulingIgnoredDuringExecution" (list (dict "weight" 100 "podAffinityTerm" $term))) -}}
{{- end -}}
{{- $affinity := mergeOverwrite $generated (deepCopy (.affinity | default dict)) -}}
{{- if $affinity }}
affinity:
  {{- toYaml $affinity | nindent 2 }}
{{- end }}
{{- $spread := $s.spread | default dict -}}
{{- if $spread.enabled }}
{{- $when := $spread.whenUnsatisfiable | default "ScheduleAnyway" -}}
{{- if not (has $when (list "ScheduleAnyway" "DoNotSchedule")) -}}
{{- fail (printf "scheduling.spread.whenUnsatisfiable must be ScheduleAnyway or DoNotSchedule (got %q) on the %s workload" $when $component) -}}
{{- end }}
{{- $skew := 1 -}}
{{- if hasKey $spread "maxSkew" -}}
{{- $skew = int $spread.maxSkew -}}
{{- if lt $skew 1 -}}
{{- fail (printf "scheduling.spread.maxSkew must be at least 1 (got %v) on the %s workload" $spread.maxSkew $component) -}}
{{- end -}}
{{- end }}
topologySpreadConstraints:
  - maxSkew: {{ $skew }}
    topologyKey: {{ $key }}
    whenUnsatisfiable: {{ $when }}
    labelSelector:
      matchLabels:
        {{- toYaml $labels | nindent 8 }}
{{- end }}
{{- end -}}

{{/*
iterion.pdbWanted — "yes" when a workload's PodDisruptionBudget must render.
Dict: enabled (auto | true | false), replicas (the replica count the
workload is GUARANTEED: the static count, or the autoscaler's floor when
one owns the count). auto renders the budget only above one replica: on a
single replica, minAvailable 1 refuses every eviction and holds a node
drain forever — a budget that protects nothing and blocks everything.
*/}}
{{- define "iterion.pdbWanted" -}}
{{- $enabled := printf "%v" .enabled -}}
{{- if eq $enabled "true" -}}
yes
{{- else if eq $enabled "false" -}}
{{- else if eq $enabled "auto" -}}
{{- if gt (int .replicas) 1 }}yes{{ end -}}
{{- else -}}
{{- fail (printf "pdb.enabled must be auto, true or false (got %q)" $enabled) -}}
{{- end -}}
{{- end -}}

{{/*
iterion.pdbBudget — the one budget line of a PodDisruptionBudget from a
{minAvailable, maxUnavailable} block: maxUnavailable wins when set (a
budget carries exactly one of the two), minAvailable defaults to 1.
*/}}
{{- define "iterion.pdbBudget" -}}
{{- $b := . | default dict -}}
{{- /* "" and null are unset (hasKey sees both as values; null survives inside a pool entry): the budget falls back to minAvailable 1, never to an EMPTY one — a PDB with neither field lets the disruption controller evict every pod. */ -}}
{{- $max := "" -}}
{{- $min := "" -}}
{{- if hasKey $b "maxUnavailable" -}}{{- $max = printf "%v" $b.maxUnavailable -}}{{- end -}}
{{- if hasKey $b "minAvailable" -}}{{- $min = printf "%v" $b.minAvailable -}}{{- end -}}
{{- if eq $max "<nil>" -}}{{- $max = "" -}}{{- end -}}
{{- if eq $min "<nil>" -}}{{- $min = "" -}}{{- end -}}
{{- if ne $max "" -}}
maxUnavailable: {{ $b.maxUnavailable }}
{{- else if ne $min "" -}}
minAvailable: {{ $b.minAvailable }}
{{- else -}}
minAvailable: 1
{{- end -}}
{{- end -}}

{{/*
iterion.promqlMatchers — refuses a value that cannot be a PromQL label
matcher list (label="v", label=~"re", …): one with no `=`, with a newline,
or with a backslash PromQL's string lexer does not know (it reads \\ \"
\a \b \f \n \r \t \v \x \u \U and octal only — a regex \d written
unescaped kills the whole rule file: write \\d, or [0-9]). Dict: value,
path (the values key, for the message). Prometheus drops a WHOLE
PrometheusRule on one unparsable expression, so a typo here would silence
every datastore alert at once.
*/}}
{{- define "iterion.promqlMatchers" -}}
{{- $v := .value | default "" -}}
{{- if or (not (regexMatch "^[^\n]+$" $v)) (not (contains "=" $v)) -}}
{{- fail (printf "%s must be a PromQL label matcher list such as pod=~\".*mongodb.*\" or job=\"x\", written as PromQL (got %q)" .path $v) -}}
{{- end -}}
{{- if not (regexMatch `^([^\\]|\\[\\"abfnrtvxuU0-7])*$` $v) -}}
{{- fail (printf "%s carries a backslash PromQL cannot read (got %q): inside a PromQL string a regex escape is written twice (\\\\d), or as a class ([0-9])" .path $v) -}}
{{- end -}}
{{- end -}}

{{/*
iterion.legal.documents — the configured legal documents as a JSON object
{slug: markdown}, keys with an empty text dropped. A key that names no
document is refused: the server would refuse the file at startup anyway
(pkg/legal.Load), and a misspelt slug is a text the operator meant to
publish. Inline documents beside config.legal.existingConfigMap are refused
too: the existing ConfigMap is what gets mounted, so the inline texts would
vanish in silence. The slug list mirrors pkg/legal.Slugs.
*/}}
{{- define "iterion.legal.documents" -}}
{{- $docs := dict -}}
{{- range $slug, $body := ((.Values.config.legal).documents | default dict) -}}
{{- if not (has $slug (list "mentions-legales" "confidentialite" "cgu" "accessibilite")) -}}
{{- fail (printf "config.legal.documents.%s names no legal document (want mentions-legales, confidentialite, cgu or accessibilite)" $slug) -}}
{{- end -}}
{{- if $body -}}
{{- $_ := set $docs $slug $body -}}
{{- end -}}
{{- end -}}
{{- if and $docs ((.Values.config.legal).existingConfigMap) -}}
{{- fail (printf "config.legal.existingConfigMap (%s) and inline config.legal.documents (%s) are both set — the existing ConfigMap is the one mounted, so the inline texts would never be published; keep one source" ((.Values.config.legal).existingConfigMap) (keys $docs | sortAlpha | join ", ")) -}}
{{- end -}}
{{- $docs | toJson -}}
{{- end -}}

{{/*
iterion.legal.mounted — non-empty when the server pod mounts a legal
documents ConfigMap: one the chart renders, or an existing one.
*/}}
{{- define "iterion.legal.mounted" -}}
{{- if or ((.Values.config.legal).existingConfigMap) (ne (include "iterion.legal.documents" .) "{}") -}}true{{- end -}}
{{- end -}}

{{- define "iterion.legal.configMapName" -}}
{{- default (printf "%s-legal" (include "iterion.fullname" .)) ((.Values.config.legal).existingConfigMap) -}}
{{- end -}}

{{- define "iterion.legal.mountPath" -}}/etc/iterion/legal{{- end -}}
