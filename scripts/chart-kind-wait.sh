#!/usr/bin/env sh
# Print the helm --timeout an install must watch: the progressDeadlineSeconds
# the install's own render gives the server Deployment, plus a 120 s margin,
# so that deadline — not a fixed client cap — decides whether a slow server
# rollout is called Failed. The kubelet serialises image pulls on a fresh
# node, so a first install can spend many minutes before the server pod even
# starts (measured on kind: 7m15 of pull time for the nats image,
# iterion#2780); a client cap smaller than the deadline makes helm give up
# while the server is still Progressing.
#
# The wait follows that one deadline. helm --wait watches every workload of
# the release, and the others keep their own: a Deployment whose deadline
# outlives the server's — values.yaml gives the runner 30000 s, a budget for
# lame-duck drains that values-dev keeps — is cut by this cap, not by its
# deadline, when it is still Progressing as the wait ends; a StatefulSet
# (the NATS sub-chart's) has no deadline, only this cap.
#
# That deadline bounds the whole server rollout for one pod only.
# Kubernetes measures progressDeadlineSeconds from the rollout's last sign
# of progress — a pod scaled up, a pod turning Ready or Available, an old
# pod scaled down — not from its start: for a single pod the one long
# stretch runs from that pod's scale-up to its turning Ready, which the
# deadline bounds, so the deadline plus the margin covers the rollout. With
# more pods each one that turns Ready restarts the clock, and the rollout
# can stay Progressing past the wait. A render whose server Deployment is
# not a single pod (spec.replicas above 1, or left out for an autoscaler)
# still gets the wait, with a warning on stderr that names the count; both
# callers install values-dev's one pod.
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
# That render is the install's own only while the templates the server
# Deployment renders through read nothing the client cannot see: no
# `.Capabilities` (off-cluster, the client's defaults stand in for the
# cluster's version and API set), no `lookup` (empty off-cluster), no
# `.Release.IsInstall` / `.Release.IsUpgrade` / `.Release.Revision` (a
# template render is always an install at revision 1). None is read today;
# TestChartKindWaitRenderReadsNoClusterState (charts/iterion) refuses one on
# that path whatever its receiver (the chart hands its includes the root as
# .root), naming the drift.
#
# Refusals are loud and name their cause: a render helm refuses; a render
# with no server Deployment (mode=local renders a StatefulSet, which has no
# such field); several candidate Deployments (the wait follows one deadline,
# and the first in render order would be a guess); a Deployment that renders
# no deadline (server.progressDeadlineSeconds unset or 0 — Kubernetes' 600 s
# default would apply and silently shrink the watch); a value other than
# plain decimal digits with no leading zero; a value beyond the int32 the
# field holds. helm hands the API the render as a YAML 1.1 decoder reads it
# (sigs.k8s.io/yaml, the one kubectl shares): a string or a fraction is
# refused, an integral float is read as its integer (6e2 is 600), a
# signed, hex, octal, binary or underscored spelling as the integer it
# denotes (+600, 0x10, 0o10, 0b11, 1_000), and a leading zero makes octal
# digits octal (010 is 8, 00 is 0) where a YAML 1.2 reader such as yq reads
# decimal (010 is 10); the API refuses a 0 or a negative. The wait is sized
# from plain digits alone, never from a spelling its readers could split
# on. Every message prints the install as its release, its chart and its
# values files, never its other arguments: a --set can carry a credential,
# and CI logs keep stderr.
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

# shown is the install as the messages print it: the release and the chart,
# the values files (-f/--values), and how many other arguments are left
# out. A --set can carry a credential (a token, a DSN) and CI logs keep
# stderr, so no other argument is printed; the userinfo of a URL is cut from
# what is.
redact_userinfo() {
  printf '%s' "$1" | sed 's#://[^/]*@#://***@#'
}
shown=
left_out=0
position=0
values_next=false
for arg in "$@"; do
  position=$((position + 1))
  if $values_next; then
    shown="$shown $(redact_userinfo "$arg")"
    values_next=false
    continue
  fi
  case $arg in
    -f|--values) shown="$shown $arg"; values_next=true ;;
    -f=*|--values=*) shown="$shown ${arg%%=*}=$(redact_userinfo "${arg#*=}")" ;;
    -f?*) shown="$shown -f$(redact_userinfo "${arg#-f}")" ;;
    -*) left_out=$((left_out + 1)) ;;
    *)
      # The release and the chart lead the list (the usage above); a word
      # after any other flag may be that flag's value.
      if [ "$position" -le 2 ] && [ "$left_out" -eq 0 ]; then
        shown="$shown $(redact_userinfo "$arg")"
      else
        left_out=$((left_out + 1))
      fi
      ;;
  esac
done
shown=${shown# }
[ "$left_out" -eq 0 ] || shown="$shown [$left_out more arguments not shown]"

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
  echo "chart-kind-wait: \`helm template $shown\` failed (helm's message above) — the wait is sized from the render the install would apply, and this install renders nothing" >&2
  exit 1
}

# read_render evaluates one yq expression over the render. A yq failure is
# a render yq cannot read, named as such; it never passes for an empty
# result.
read_render() {
  printf '%s\n' "$render" | yq ea "$1" || {
    echo "chart-kind-wait: yq could not read the render of \`helm template $shown\` with '$1' (yq's message above)" >&2
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
    echo "chart-kind-wait: the render of \`helm template $shown\` carries no Deployment labelled app.kubernetes.io/component=server (the label is on: ${labelled:-nothing}) — mode=local renders the server as a StatefulSet, which has no progressDeadlineSeconds, so the install's wait has no deadline to follow; this helper sizes cloud-mode installs" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: the render of \`helm template $shown\` carries $# Deployments labelled app.kubernetes.io/component=server ($(printf '%s' "$candidates" | tr '\n' ' ' | sed 's/ $//; s/ /, /g')) — the wait follows one Deployment's deadline, and the first in render order would be a guess; keep one server Deployment in the release" >&2
    exit 1
    ;;
esac
name=$1

# The field as rendered: its YAML tag tells an integer or a float from the
# string a template prints verbatim, and a null is the field left out. A
# number is then judged on its spelling, the text the API's decoder reads:
# only plain decimal digits with no leading zero size a wait (the header
# says what the decoder makes of the others).
tag=$(read_render "[.] | map(select($server)) | .[0].spec.progressDeadlineSeconds | tag") || exit 1
deadline=$(read_render "[.] | map(select($server)) | .[0].spec.progressDeadlineSeconds") || exit 1
case $tag in
  '!!int'|'!!float') ;;
  '!!null')
    echo "chart-kind-wait: Deployment/$name renders no spec.progressDeadlineSeconds for \`helm template $shown\` — server.progressDeadlineSeconds is unset or 0 in the values the install reads (the template renders the field through a \`with\` that skips both), so the install would silently watch Kubernetes' 600 s default; set a real deadline in the values the install reads" >&2
    exit 1
    ;;
  *)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline ($tag) for \`helm template $shown\` — not an integer, so the API refuses the Deployment and no install can carry it; set an integer deadline" >&2
    exit 1
    ;;
esac
# A leading zero is judged before the float refusal: yq tags 010 an
# integer and 08 a float, and both are the same misspelling.
case $deadline in
  0)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: 0 for \`helm template $shown\` — the API refuses a deadline that does not exceed minReadySeconds, and 0 budgets no watch; set a real deadline" >&2
    exit 1
    ;;
  0*[!0-9]*) ;; # a base prefix or a fraction (0x10, 0.5), judged below
  0*)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline for \`helm template $shown\` — not a positive decimal integer: a leading zero, which helm's YAML 1.1 decoding (the form the API receives) reads as octal where the digits allow (010 is 8, 00 is 0) and a YAML 1.2 reader such as yq as decimal (010 is 10), so no wait is sized from it; set the deadline without the leading zero" >&2
    exit 1
    ;;
esac
if [ "$tag" = '!!float' ]; then
  echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline (!!float) for \`helm template $shown\` — not an integer spelling: helm's YAML 1.1 decoding (the form the API receives) reads an integral float as its integer (6e2 is 600) and a fraction is refused, but the wait is sized from plain decimal digits alone; helm prints a values number of a million or more as such a float (1e+06), so quote that number in the values, or set an integer deadline" >&2
  exit 1
fi
case $deadline in
  ''|*[!0-9]*)
    echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline for \`helm template $shown\` — not a positive decimal integer: the API refuses a negative deadline, and helm's YAML 1.1 decoding (the form the API receives) reads a signed, hex, octal, binary or underscored spelling (+600, 0x10, 0o10, 0b11, 1_000) as the integer it denotes, which this helper does not reimplement; set the deadline as plain decimal digits" >&2
    exit 1
    ;;
esac
if [ ${#deadline} -gt 10 ] || [ "$deadline" -gt 2147483647 ]; then
  echo "chart-kind-wait: Deployment/$name renders spec.progressDeadlineSeconds: $deadline for \`helm template $shown\` — beyond the int32 the field holds (2147483647 at most), so no install can carry it and the printed wait would not be a deadline the install runs with" >&2
  exit 1
fi

# One pod is what the deadline bounds (the header says why): a server of
# several pods, or of a count an autoscaler owns, gets the wait and a
# warning, so an install that can outlast it is not sized in silence. A 0
# has no rollout to watch.
replicas=$(read_render "[.] | map(select($server)) | .[0].spec.replicas") || exit 1
case $replicas in
  0|1) ;;
  null)
    echo "chart-kind-wait: warning: Deployment/$name renders no spec.replicas for \`helm template $shown\` — the count is left to whatever scales the Deployment (this chart's server HPA, server.hpa.enabled), and each pod added during the rollout restarts the deadline when it turns Ready, so the rollout can stay Progressing past this wait of one deadline plus the margin" >&2
    ;;
  *)
    echo "chart-kind-wait: warning: Deployment/$name renders spec.replicas: $replicas for \`helm template $shown\` — each pod that turns Ready restarts the deadline, so the rollout can stay Progressing past this wait of one deadline plus the margin" >&2
    ;;
esac

# The margin keeps helm watching past the moment the server's deadline can
# fire: for one server pod, the client never gives up on the server before
# Kubernetes could call its rollout Failed. The other workloads' deadlines
# are not followed (the header says which). Within int32 the sum stays exact
# in shell arithmetic.
margin=120
echo "$((deadline + margin))s"
