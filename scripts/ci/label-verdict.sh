#!/usr/bin/env bash
# label-verdict — the verdict of a tests.yml run that a label asking for no CI
# started (#3738, pull request CI on demand).
#
# tests.yml listens to `labeled` so that the `ci:run` and `ci:full` labels
# start a run, and GitHub fires `labeled` for EVERY label: `epic:…`, renovate's
# `dependencies` a second after it opens its pull request. Such a run runs
# nothing, yet it still writes check runs under the required names, and a job
# its `if:` skips reports SUCCESS: a run of skipped jobs would turn the red
# `test` of the run before it green. So a run started by any other label
# REPEATS the verdict of the run before it on the same commit, and does
# nothing else: tests.yml queues it behind the pull request's run in progress
# (it never cancels one), so when it starts the run before it has concluded.
#
# The run it repeats is the NEWEST earlier run of tests.yml on this head
# commit, triggered by pull_request, that reached a verdict — sorted here by
# (created_at, id), never trusted from the API's order. A cancelled run
# decided nothing and is passed over (a newer run replaced it, or someone
# stopped it); a run of an earlier label repeats the one before it in turn.
# Its verdict, read from its latest attempt:
#
#   test  success when its `test`, `vendor-check`, `golangci` and `fmt-check`
#         jobs each concluded success or skipped — the required contexts this
#         run skips; `brand`, the other one, runs again in this run;
#   race  success when its `race` job did.
#
# Anything else is failure. A job missing from that run is failure too.
#
# Inputs (env): GITHUB_REPOSITORY, GITHUB_RUN_ID, HEAD_SHA (the pull request's
# head commit), LABEL (the label that started this run), GITHUB_OUTPUT, GH_TOKEN
# for `gh`. Output: `test=` and `race=` appended to $GITHUB_OUTPUT, and the run
# repeated as a `::notice::`. Exit 0 once it answered. Exit 1 with an
# `::error::` when it cannot answer — the history is unreadable, an earlier
# run is still in progress, or no earlier run reached a verdict: the job fails,
# its outputs stay empty, and both aggregators read an empty verdict as red.
set -euo pipefail

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set: the verdict has nowhere to go}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set: no run history to read}"
: "${HEAD_SHA:?HEAD_SHA is not set: no commit whose runs to read}"

ways_out="push a commit, remove and re-add the \`ci:run\` label (labelling starts a run of the light layer), or land the pull request through the train (\`task ci:train\`, scripts/ci/train)"

cannot() {
  echo "::error::the label '${LABEL:-}' asks for no CI, so this run repeats the verdict of the run before it on ${HEAD_SHA}, and it cannot: $1. The required checks stay red until a run reaches a verdict: ${ways_out}."
  exit 1
}

run_id="${GITHUB_RUN_ID:-}"
if ! [[ "$run_id" =~ ^[0-9]{1,18}$ ]]; then
  cannot "GITHUB_RUN_ID is '${run_id}', not a run id"
fi

if ! self_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}" 2>&1)"; then
  cannot "this run is unreadable (${self_json})"
fi
if ! self="$(jq -er '"\(.workflow_id) \(.created_at)"' <<<"$self_json" 2>&1)"; then
  cannot "this run is unreadable (${self})"
fi
read -r workflow_id created_at <<<"$self"

if ! runs_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/${workflow_id}/runs?head_sha=${HEAD_SHA}&event=pull_request&per_page=100" 2>&1)"; then
  cannot "the runs of this commit are unreadable (${runs_json})"
fi
# Earlier than this run by (created_at, id), newest first.
if ! runs="$(jq -r --arg created "$created_at" --argjson self "$run_id" '
  [.workflow_runs[]
   | select(.id != $self and ([.created_at, .id] < [$created, $self]))
   | [.created_at, .id, .status, (.conclusion // "none"), (.html_url // "")]]
  | sort | reverse | .[] | "\(.[1]) \(.[2]) \(.[3]) \(.[4])"' <<<"$runs_json" 2>&1)"; then
  cannot "the runs of this commit are unreadable (${runs})"
fi

# verdict NAME... prints `success`, or the first job that is not a success or
# a skip, with its conclusion, from the jobs JSON on stdin.
verdict() {
  jq -r --args '
    . as $doc
    | [$ARGS.positional[] as $name
       | [$doc.jobs[] | select(.name == $name)] as $jobs
       | if ($jobs | length) == 0 then "\($name) is missing"
         elif ($jobs | all(.conclusion == "success" or .conclusion == "skipped")) then empty
         else "\($name) concluded \([$jobs[] | (.conclusion // "nothing")] | join(","))" end]
    | if length == 0 then "success" else .[0] end' "$@"
}

while read -r id status conclusion url; do
  [ -n "$id" ] || continue
  if [ "$status" != completed ]; then
    cannot "run ${id} (${url}) is still ${status}"
  fi
  if [ "$conclusion" = cancelled ]; then
    echo "::notice::run ${id} (${url}) was cancelled: it decided nothing, passed over"
    continue
  fi
  if ! jobs_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${id}/jobs?filter=latest&per_page=100" 2>&1)"; then
    cannot "the jobs of run ${id} are unreadable (${jobs_json})"
  fi
  if ! test_verdict="$(verdict test vendor-check golangci fmt-check <<<"$jobs_json" 2>&1)"; then
    cannot "the jobs of run ${id} are unreadable (${test_verdict})"
  fi
  if ! race_verdict="$(verdict race <<<"$jobs_json" 2>&1)"; then
    cannot "the jobs of run ${id} are unreadable (${race_verdict})"
  fi
  test_out=success race_out=success
  [ "$test_verdict" = success ] || test_out=failure
  [ "$race_verdict" = success ] || race_out=failure
  {
    echo "test=${test_out}"
    echo "race=${race_out}"
  } >>"$GITHUB_OUTPUT"
  echo "::notice::the label '${LABEL:-}' asks for no CI: this run repeats run ${id} (${url}), concluded ${conclusion} — test: ${test_verdict}; race: ${race_verdict}"
  exit 0
done <<<"$runs"

cannot "no earlier run of tests.yml on this commit reached a verdict"
