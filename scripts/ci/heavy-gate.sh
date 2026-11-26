#!/usr/bin/env bash
# heavy-gate — decides whether a push to main runs tests.yml's heavy layer
# (race-shard, test-e2e, mongo-conformance, cloud-e2e), the debounced push of
# docs/adr/173-experimental-delivery-mode.md (#3737).
#
# A push stays light only when a GREEN HEAVY RUN of tests.yml on main landed
# in the last HEAVY_EVERY_HOURS hours; every other push runs the heavy layer.
# A green heavy run is one predicate, the one version.yml's release gate
# reads (#3741):
#
#   - a run of this repository (head_repository is GITHUB_REPOSITORY: a fork's
#     pull request from a branch named `main` is not one), triggered by push,
#     schedule or workflow_dispatch;
#   - created inside the window plus two hours, so a rerun of an old run —
#     which proves an old sha — does not count;
#   - whose latest attempt concluded `success`;
#   - and whose latest attempt holds at least one `race-shard (N)` job, every
#     one of them concluded `success` and completed inside the window. A
#     cancelled, skipped, failed or timed-out shard proved nothing.
#
# The NEWEST run whose heavy legs ran decides, not any run of the window: a
# red heavy run after a green one makes the push heavy. So while the heavy
# layer is red, or has not run green, every push stays heavy until a green
# heavy run lands: a red heavy run never buys a light window.
#
# A RERUN keeps the heavy layer: when GITHUB_RUN_ATTEMPT is not 1 the script
# decides heavy without reading the history. The history lists completed runs
# only, so it cannot hold the run being rerun: a rerun of a red heavy run would
# otherwise re-decide light behind an older green one, erase its own red, and
# hand main-watch a green attempt to read as a flake.
#
# The decision fails toward MORE testing, never silently toward less: when the
# run history cannot be read (API error, unreadable JSON) or the window is not
# a whole number of hours in 0..720, it says so with a `::warning::` and
# decides heavy. HEAVY_EVERY_HOURS=0 is the escape hatch back to a heavy run on
# every push.
#
# Inputs (env): HEAVY_EVERY_HOURS (the repository variable
# CI_HEAVY_EVERY_HOURS, defaulted by the workflow), GITHUB_RUN_ATTEMPT (set by
# the runner; unset reads as a first attempt), GITHUB_REPOSITORY,
# GITHUB_OUTPUT, GH_TOKEN for `gh`. Output: `heavy=true` or `heavy=false`
# appended to $GITHUB_OUTPUT, and the reason as a `::notice::`. Exit 0 on every
# decision; non-zero only when the output itself cannot be written — the
# heavy legs read anything but `false` as heavy, so even that runs them.
#
# JUDGE MODE — `heavy-gate.sh --judge-run <run_id> <attempt>`, version.yml's
# release gate (#3741): is that attempt of a tests.yml run a green heavy run?
# The same predicate, minus the window (a release judges the run that just
# completed, whenever its shards finished): this repository's push, schedule
# or dispatch run, the attempt concluded `success`, and at least one
# `race-shard (N)` job, every one concluded `success`. Prints exactly one line
# on stdout, `green` or `not-green`, with the reason as a `::notice::` on
# stderr; exits 0 on a decision and 2 when the attempt or its jobs cannot be
# read — a release never decides on a guess. Inputs: GITHUB_REPOSITORY,
# GH_TOKEN.
set -euo pipefail

# The predicate, ONE definition for both modes (jq, prepended to every
# program that judges a run):
#   heavy_candidate($repo) — a run object: this repository's push, schedule
#     or dispatch run;
#   shard_verdict($cutoff) — a jobs listing: `light` when every
#     `race-shard (N)` job skipped (or there is none: the `race` aggregator and
#     any job whose name merely starts alike are not shards), `green` when
#     every one concluded success with completed_at >= $cutoff ("" for any
#     time), `red` otherwise.
# A green heavy run is a heavy_candidate whose attempt concluded success and
# whose shard_verdict is green: green_heavy below.
# shellcheck disable=SC2016 # $repo and $cutoff are jq variables, not shell ones.
heavy_predicate='
def heavy_candidate($repo):
  (.event == "push" or .event == "schedule" or .event == "workflow_dispatch")
  and (.head_repository.full_name == $repo);
def shard_verdict($cutoff):
  [.jobs[] | select(.name | test("^race-shard \\([0-9]+\\)$"))] as $shards
  | if ($shards | all(.conclusion == "skipped")) then "light"
    elif ($shards | all(.conclusion == "success" and ((.completed_at // "") >= $cutoff))) then "green"
    else "red" end;
'

# green_heavy <run conclusion> <shard verdict>
green_heavy() { [ "$1" = success ] && [ "$2" = green ]; }

if [ "${1:-}" = "--judge-run" ]; then
  : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set: no run to read}"
  unreadable() {
    echo "::error::heavy-gate --judge-run: $*" >&2
    exit 2
  }
  judged() {
    echo "::notice::run ${run_id} attempt ${attempt}: ${1} — $2" >&2
    echo "$1"
    exit 0
  }
  [ "$#" -eq 3 ] || unreadable "usage: heavy-gate.sh --judge-run <run_id> <attempt>"
  run_id="$2"
  attempt="$3"
  [[ "$run_id" =~ ^[0-9]{1,20}$ ]] || unreadable "run id '${run_id}' is not a number"
  [[ "$attempt" =~ ^[0-9]{1,6}$ ]] || unreadable "attempt '${attempt}' is not a number"
  base="repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}/attempts/${attempt}"
  run_json="$(gh api "$base" 2>&1)" || unreadable "cannot read ${base} (${run_json})"
  facts="$(jq -r --arg repo "$GITHUB_REPOSITORY" "${heavy_predicate}"'
    if heavy_candidate($repo) then "candidate \(.conclusion // "none")" else "foreign \(.event) \(.head_repository.full_name)" end' <<<"$run_json" 2>&1)" ||
    unreadable "cannot read ${base} (${facts})"
  jobs_json="$(gh api "${base}/jobs?per_page=100" 2>&1)" || unreadable "cannot read the jobs of ${base} (${jobs_json})"
  verdict="$(jq -r "${heavy_predicate}"'shard_verdict("")' <<<"$jobs_json" 2>&1)" ||
    unreadable "cannot read the jobs of ${base} (${verdict})"
  read -r kind conclusion rest <<<"$facts"
  if [ "$kind" != candidate ]; then
    judged not-green "a ${conclusion} run of ${rest}, not a push, schedule or dispatch run of ${GITHUB_REPOSITORY}"
  fi
  if green_heavy "$conclusion" "$verdict"; then
    judged green "concluded success with every race-shard (N) job a success"
  fi
  judged not-green "concluded ${conclusion}, race shards ${verdict} (light: none ran)"
fi

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set: the decision has nowhere to go}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set: no run history to read}"

# maxHours bounds the window: a month. A longer one is a typo, not a choice.
max_hours=720

decide() {
  echo "heavy=$1" >>"$GITHUB_OUTPUT"
  echo "::notice::heavy=$1 — $2"
  exit 0
}

hours="${HEAVY_EVERY_HOURS:-}"
# Four digits at most before any arithmetic: a longer string overflows bash's
# integers and would turn the window into a negative or wrapped number.
if ! [[ "$hours" =~ ^[0-9]{1,4}$ ]] || [ "$((10#$hours))" -gt "$max_hours" ]; then
  echo "::warning::CI_HEAVY_EVERY_HOURS is '${hours}', not a whole number of hours in 0..${max_hours} — running the heavy layer rather than guessing a window."
  decide true "the heavy window is unreadable"
fi
hours=$((10#$hours))
if [ "$hours" -eq 0 ]; then
  decide true "CI_HEAVY_EVERY_HOURS is 0: every push to main runs the heavy layer"
fi

if [ "${GITHUB_RUN_ATTEMPT:-1}" != 1 ]; then
  decide true "attempt ${GITHUB_RUN_ATTEMPT} reruns this run: a rerun keeps the heavy layer rather than re-deciding behind a history that cannot hold the run itself"
fi

now="$(date -u +%s)"
cutoff="$(date -u -d "@$((now - hours * 3600))" +%Y-%m-%dT%H:%M:%SZ)"
created_cutoff="$(date -u -d "@$((now - (hours + 2) * 3600))" +%Y-%m-%dT%H:%M:%SZ)"

# The most recent completed runs on main that can witness the heavy layer,
# NEWEST FIRST by (created_at, id) — sorted here, never trusted from the API's
# order. The newest one whose heavy legs ran decides: green inside the window
# keeps this push light; anything else — red, cancelled, timed out, a run
# that failed elsewhere — makes it heavy, even when an older green run sits
# inside the window. A run whose shards all skipped (a light push) is passed
# over.
if ! runs_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/tests.yml/runs?branch=main&status=completed&per_page=100" 2>&1)"; then
  echo "::warning::cannot list tests.yml runs on main (${runs_json}) — running the heavy layer rather than skipping it on an unread history."
  decide true "the run history is unreadable"
fi
if ! runs="$(jq -r --arg repo "$GITHUB_REPOSITORY" --arg cutoff "$cutoff" --arg created "$created_cutoff" "${heavy_predicate}"'
  [.workflow_runs[]
   | select(heavy_candidate($repo))
   | select((.created_at // "") >= $created and (.updated_at // "") >= $cutoff)
   | [.created_at, .id, (.conclusion // "none")]]
  | sort | reverse | .[] | "\(.[1]) \(.[2])"' <<<"$runs_json" 2>&1)"; then
  echo "::warning::cannot read the tests.yml run list (${runs}) — running the heavy layer rather than skipping it on an unread history."
  decide true "the run history is unreadable"
fi

while read -r id conclusion; do
  [ -n "$id" ] || continue
  if ! jobs_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${id}/jobs?filter=latest&per_page=100" 2>&1)"; then
    echo "::warning::cannot list the jobs of run ${id} (${jobs_json}) — running the heavy layer rather than skipping it on an unread history."
    decide true "the run history is unreadable"
  fi
  # verdict: light (every shard skipped, or none), green, or red — inside the
  # window: a shard must have completed after the cutoff.
  if ! verdict="$(jq -r --arg cutoff "$cutoff" "${heavy_predicate}"'shard_verdict($cutoff)' <<<"$jobs_json" 2>&1)"; then
    echo "::warning::cannot read the jobs of run ${id} (${verdict}) — running the heavy layer rather than skipping it on an unread history."
    decide true "the run history is unreadable"
  fi
  [ "$verdict" != light ] || continue
  if green_heavy "$conclusion" "$verdict"; then
    decide false "run ${id} is a green heavy run within the last ${hours} h (since ${cutoff}); this push stays light"
  fi
  decide true "run ${id}, the newest heavy run, is not green (shards ${verdict}, concluded ${conclusion}); this push runs the heavy layer until a green heavy run lands"
done <<<"$runs"

decide true "no green heavy run of tests.yml on main within the last ${hours} h (since ${cutoff})"
