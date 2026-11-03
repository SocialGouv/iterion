#!/usr/bin/env sh
# Print the helm --timeout an install must watch: the progressDeadlineSeconds
# the install's own render gives the server Deployment, plus a 120 s margin,
# so the deadline — not a fixed client cap — decides whether a slow install
# is called Failed. The kubelet serialises image pulls on a fresh node, so a
# first install can spend many minutes before the server pod even starts
# (measured on kind: 7m15 of pull time for the nats image, iterion#2780); a
# client cap smaller than the deadline makes helm give up while the workload
# is still Progressing.
#
# The deadline is read from the manifest helm renders for the install's own
# arguments — `helm template <release> <chart> [-f …] [--set …]`, the very
# arguments the install hands to `helm install` / `helm upgrade` — never
# from a values file. What the API server receives is the rendered
# Deployment, and the render already applies everything that stands between
# a values file and that field: go-yaml's reading of the file (block
# scalars, flow collections, directives, quoted keys, repeated blocks), the
# `--set` overrides, the chart's own `with` that skips an unset or 0 knob,
# the StatefulSet of mode=local. A reader of the values file would have to
# reimplement all of that and lose to the next form; this helper reads what
# helm produced. The server Deployment is the one the chart labels
# app.kubernetes.io/component=server (the label the Service selects the
# server pods with), read by yq (mikefarah/yq v4).
#
# Refusals are loud and name their cause: a render helm refuses; a render
# with no server Deployment (mode=local renders a StatefulSet, which has no
# such field); several candidate Deployments (the wait follows one deadline,
# and the first in render order would be a guess); a Deployment that renders
# no deadline (server.progressDeadlineSeconds unset or 0 — Kubernetes' 600 s
# default would apply and silently shrink the watch); a value that is not a
# positive integer (the API refuses the Deployment, or no watch can be
# budgeted from it); a value beyond the int32 the field holds.
#
# Usage: scripts/chart-kind-wait.sh <release> <chart> [helm args…]
# The arguments are the install's own — release, chart, every -f/--values,
# --set and --namespace — so the helper and the install read one list; the
# two install sites (Taskfile chart:kind, the cloud-e2e job) build that list
# once and pass it to both.
set -eu

[ $# -ge 2 ] || {
  echo "usage: chart-kind-wait.sh <release> <chart> [helm args…] — the arguments the install passes to helm" >&2
  exit 2
}
args=$*

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "chart-kind-wait: $tool is not in PATH — the wait is sized from the render helm produces and yq reads; devbox provides both (kubernetes-helm, yq-go)" >&2
    exit 1
  }
done
# The expressions below are mikefarah/yq v4 syntax; the python wrapper of
# the same name reads jq syntax and would fail on them for a reason it
# cannot name, so the implementation is checked before the render is.
yq_version=$(yq --version 2>&1 || true)
case $yq_version in
  *"mikefarah/yq"*"version v4."*) ;;
  *)
    echo "chart-kind-wait: yq at $(command -v yq) is not mikefarah/yq v4 (\`yq --version\` says: $yq_version) — the render is read with its expressions; put yq-go on PATH" >&2
    exit 1
    ;;
esac

# The render the install would apply. helm's own message stays on stderr:
# it names the cause (a missing sub-chart, a value the schema refuses, a
# template error), the line below only says what the failure means here.
render=$(helm template "$@") || {
  echo "chart-kind-wait: \`helm template $args\` failed (helm's message above) — the wait is sized from the render the install would apply, and this install renders nothing" >&2
  exit 1
}

# read_render evaluates one yq expression over the render. A yq failure is
# a render yq cannot read, named as such; it never passes for an empty
# result.
read_render() {
  printf '%s\n' "$render" | yq ea "$1" || {
    echo "chart-kind-wait: yq could not read the render of \`helm template $args\` with '$1' (yq's message above)" >&2
    exit 1
  }
}

# The candidates: every Deployment the render labels component=server, by
# name, one per line. Per-element string building goes through `|=`: yq's
# `+` over a multi-element context cross-multiplies its operands.
server='.kind == "Deployment" and .metadata.labels["app.kubernetes.io/component"] == "server"'
candidates=$(read_render "[.] | map(select($server)) | .[].metadata.name") || exit 1
# shellcheck disable=SC2086 # the names split on purpose: one word each
set -- $candidates
case $# in
  1) ;;
  0)
    labelled=$(read_render '[.] | map(select(.metadata.labels["app.kubernetes.io/component"] == "server")) | .[] |= (.kind + "/" + .metadata.name) | join(", ")') || exit 1
    echo "chart-kind-wait: the render of \`helm template $args\` carries no Deployment labelled app.kubernetes.io/component=server (the label is on: ${labelled:-nothing}) — mode=local renders the server as a StatefulSet, which has no progressDeadlineSeconds, so the install's wait has no deadline to follow; this helper sizes cloud-mode installs" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: the render of \`helm template $args\` carries $# Deployments labelled app.kubernetes.io/component=server ($(printf '%s' "$candidates" | tr '\n' ' ' | sed 's/ $//; s/ /, /g')) — the wait follows one Deployment's deadline, and the first in render order would be a guess; keep one server Deployment in the release" >&2
    exit 1
    ;;
esac
name=$1

# The field as rendered: its YAML tag tells an integer from the string or
# float a template prints verbatim, and a null is the field left out.
tag=$(read_render "[.] | map(select($server)) | .[0].spec.progressDeadlineSeconds | tag") || exit 1
deadline=$(read_render "[.] | map(select($server)) | .[0].spec.progressDeadlineSeconds") || exit 1
case $tag in
  '!!int') ;;
  '!!null')
    echo "chart-kind-wait: Deployment/$name renders no spec.progressDeadlineSeconds for \`helm template $args\` — server.progressDeadlineSeconds is unset or 0 in the values the install reads (the template renders the field through a \`with\` that skips both), so the install would silently watch Kubernetes' 600 s default; set a real deadline in the values the install reads" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline ($tag) for \`helm template $args\` — not an integer, so the API refuses the Deployment and no install can carry it; set an integer deadline" >&2
    exit 1
    ;;
esac
case $deadline in
  ''|*[!0-9]*)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline for \`helm template $args\` — not a positive decimal integer, so it budgets no watch (the API refuses a negative deadline); set a positive integer deadline" >&2
    exit 1
    ;;
  0)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: 0 for \`helm template $args\` — the API refuses a deadline that does not exceed minReadySeconds, and 0 budgets no watch; set a real deadline" >&2
    exit 1
    ;;
esac
if [ ${#deadline} -gt 10 ] || [ "$deadline" -gt 2147483647 ]; then
  echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline for \`helm template $args\` — beyond the int32 the field holds (2147483647 at most), so no install can carry it and the printed wait would not be a deadline the install runs with" >&2
  exit 1
fi

# The deadline budgets the server Deployment alone; the wait also covers the
# stores and the release plumbing, and helm needs a breath after the moment
# the deadline can fire. Within int32 the sum stays exact in shell
# arithmetic.
margin=120
echo "$((deadline + margin))s"
