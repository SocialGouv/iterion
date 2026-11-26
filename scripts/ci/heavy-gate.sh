#!/usr/bin/env bash
# heavy-gate — decides whether a push to main runs tests.yml's heavy layer
# (race-shard, test-e2e, mongo-conformance, cloud-e2e), the debounced push of
# docs/adr/173-experimental-delivery-mode.md (#3737).
#
# A push runs the heavy layer when no run of tests.yml on main ran it in the
# last HEAVY_EVERY_HOURS hours; every other push stays light. "Ran it" means a
# `race-shard` job of the run's latest attempt CONCLUDED success or failure and
# completed inside the window: a skipped or cancelled heavy leg proved nothing
# and does not count. The window is measured from the heavy leg's completion,
# so a push run, the nightly or a dispatch that ran the heavy layer each count.
#
# The decision fails toward MORE testing, never silently toward less: when the
# run history cannot be read (API error, unreadable JSON) or the window is not
# a whole number of hours, it says so with a `::warning::` and decides heavy.
# HEAVY_EVERY_HOURS=0 is the escape hatch back to a heavy run on every push.
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

decide() {
  echo "heavy=$1" >>"$GITHUB_OUTPUT"
  echo "::notice::heavy=$1 — $2"
  exit 0
}

hours="${HEAVY_EVERY_HOURS:-}"
if ! [[ "$hours" =~ ^[0-9]+$ ]]; then
  echo "::warning::CI_HEAVY_EVERY_HOURS is '${hours}', not a whole number of hours — running the heavy layer rather than guessing a window."
  decide true "the heavy window is unreadable"
fi
hours=$((10#$hours))
if [ "$hours" -eq 0 ]; then
  decide true "CI_HEAVY_EVERY_HOURS is 0: every push to main runs the heavy layer"
fi

cutoff="$(date -u -d "@$(($(date -u +%s) - hours * 3600))" +%Y-%m-%dT%H:%M:%SZ)"

# The most recent completed runs on main, any event. A run whose last update
# is older than the cutoff cannot hold a heavy leg completed inside the window.
if ! runs_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/tests.yml/runs?branch=main&status=completed&per_page=100" 2>&1)"; then
  echo "::warning::cannot list tests.yml runs on main (${runs_json}) — running the heavy layer rather than skipping it on an unread history."
  decide true "the run history is unreadable"
fi
if ! run_ids="$(jq -r --arg cutoff "$cutoff" '.workflow_runs[] | select(.updated_at >= $cutoff) | .id' <<<"$runs_json" 2>&1)"; then
  echo "::warning::cannot read the tests.yml run list (${run_ids}) — running the heavy layer rather than skipping it on an unread history."
  decide true "the run history is unreadable"
fi

for id in $run_ids; do
  if ! jobs_json="$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${id}/jobs?filter=latest&per_page=100" 2>&1)"; then
    echo "::warning::cannot list the jobs of run ${id} (${jobs_json}) — running the heavy layer rather than skipping it on an unread history."
    decide true "the run history is unreadable"
  fi
  if ! ran="$(jq -r --arg cutoff "$cutoff" '[.jobs[] | select((.name | startswith("race-shard")) and (.conclusion == "success" or .conclusion == "failure") and ((.completed_at // "") >= $cutoff))] | length' <<<"$jobs_json" 2>&1)"; then
    echo "::warning::cannot read the jobs of run ${id} (${ran}) — running the heavy layer rather than skipping it on an unread history."
    decide true "the run history is unreadable"
  fi
  if [ "$ran" -gt 0 ]; then
    decide false "run ${id} ran the heavy layer within the last ${hours} h (since ${cutoff}); this push stays light"
  fi
done

decide true "no tests.yml run on main ran the heavy layer within the last ${hours} h (since ${cutoff})"
