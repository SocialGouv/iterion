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
# The decision fails toward MORE testing, never silently toward less: when the
# run history cannot be read (API error, unreadable JSON) or the window is not
# a whole number of hours in 0..720, it says so with a `::warning::` and
# decides heavy. HEAVY_EVERY_HOURS=0 is the escape hatch back to a heavy run on
# every push.
#
# Inputs (env): HEAVY_EVERY_HOURS (the repository variable
# CI_HEAVY_EVERY_HOURS, defaulted by the workflow), GITHUB_REPOSITORY,
# GITHUB_OUTPUT, GH_TOKEN for `gh`. Output: `heavy=true` or `heavy=false`
# appended to $GITHUB_OUTPUT, and the reason as a `::notice::`. Exit 0 on every
# decision; non-zero only when the output itself cannot be written — the
# heavy legs read anything but `false` as heavy, so even that runs them.
set -euo pipefail

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
if ! runs="$(jq -r --arg repo "$GITHUB_REPOSITORY" --arg cutoff "$cutoff" --arg created "$created_cutoff" '
  [.workflow_runs[]
   | select(.event == "push" or .event == "schedule" or .event == "workflow_dispatch")
   | select(.head_repository.full_name == $repo)
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
  # Exactly the matrix legs GitHub names `race-shard (N)`: the `race`
  # aggregator and any other job whose name merely starts alike are not shards.
  # verdict: light (every shard skipped, or none), green, or red.
  if ! verdict="$(jq -r --arg cutoff "$cutoff" '
    [.jobs[] | select(.name | test("^race-shard \\([0-9]+\\)$"))] as $shards
    | if ($shards | all(.conclusion == "skipped")) then "light"
      elif ($shards | all(.conclusion == "success" and ((.completed_at // "") >= $cutoff))) then "green"
      else "red" end' <<<"$jobs_json" 2>&1)"; then
    echo "::warning::cannot read the jobs of run ${id} (${verdict}) — running the heavy layer rather than skipping it on an unread history."
    decide true "the run history is unreadable"
  fi
  case "$verdict" in
    light) continue ;;
    green)
      if [ "$conclusion" = success ]; then
        decide false "run ${id} is a green heavy run within the last ${hours} h (since ${cutoff}); this push stays light"
      fi
      decide true "run ${id}, the newest heavy run, ran green shards but concluded ${conclusion}; this push runs the heavy layer until a green heavy run lands"
      ;;
    *) decide true "run ${id}, the newest heavy run, is not green (${verdict}, concluded ${conclusion}); this push runs the heavy layer until a green heavy run lands" ;;
  esac
done <<<"$runs"

decide true "no green heavy run of tests.yml on main within the last ${hours} h (since ${cutoff})"
