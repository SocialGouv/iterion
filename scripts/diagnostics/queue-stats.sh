#!/usr/bin/env bash
# Measure main's merge queue over a window of days:
#   - how long a pull request waits from its first enqueue to its merge, and
#     how many merges skipped the queue (admin bypass);
#   - how many queue builds (Tests workflow, merge_group) the window paid for,
#     and why each rebuild happened — in particular builds thrown away because
#     main moved under them: a direct push (a release, a pull request merged
#     without the queue, any other push) invalidates every merge group in
#     flight, and GitHub rebuilds each one from scratch;
#   - which tests failed the queue builds that failed.
# Usage: scripts/diagnostics/queue-stats.sh [--since YYYY-MM-DD] [--until YYYY-MM-DD]
#                                           [--repo OWNER/NAME] [--no-logs]
# Read-only. Needs an authenticated `gh`, `jq` and GNU `date`. The window is
# inclusive, in UTC: --since defaults to seven days ago, --until to today — fix
# both to compare two periods. --no-logs skips the job-log downloads behind the
# failing-test table, the slowest part. docs/merge-policy.md says how to read
# the output.
set -euo pipefail

since=""
until=""
repo="SocialGouv/iterion"
logs=1
while [ $# -gt 0 ]; do
  case "$1" in
    --since) since="${2:?--since needs a YYYY-MM-DD date}"; shift 2 ;;
    --until) until="${2:?--until needs a YYYY-MM-DD date}"; shift 2 ;;
    --repo) repo="${2:?--repo needs OWNER/NAME}"; shift 2 ;;
    --no-logs) logs=0; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "queue-stats: unknown argument: $1" >&2; exit 2 ;;
  esac
done

for tool in gh jq; do
  command -v "$tool" >/dev/null || { echo "queue-stats: $tool is required" >&2; exit 2; }
done
[ -n "$since" ] || since="$(date -u -d '7 days ago' +%F)"
[ -n "$until" ] || until="$(date -u +%F)"
for d in "$since" "$until"; do
  # A date GNU date does not print back unchanged is not a date: 2026-09-31
  # would otherwise become an empty window and an all-zero report.
  if [ "$(date -u -d "$d" +%F 2>/dev/null || true)" != "$d" ]; then
    echo "queue-stats: not a YYYY-MM-DD date: '$d'" >&2
    exit 2
  fi
done
if [[ "$since" > "$until" ]]; then
  echo "queue-stats: --since $since is after --until $until" >&2
  exit 2
fi
# A queue build can sit on a base committed shortly before the window opens:
# commits and merged pull requests are read from three days earlier, so those
# bases can still be named.
bases_since="$(date -u -d "$since - 3 days" +%F)"

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

# gh >= 2.9x refuses to print a job log (it carries terminal escape sequences)
# unless told to; older versions have neither the check nor the flag. Read the
# help into a file: `grep -q` on a pipe closes it early, and under pipefail the
# SIGPIPE'd gh would turn a match into a failure.
log_flags=()
gh api --help > "$work/gh-api-help" 2>&1
if grep -q -- '--allow-escape-sequences' "$work/gh-api-help"; then
  log_flags=(--allow-escape-sequences)
fi

pct() { # pct <file of numbers> -> "p50 … max (n=N)", nearest-rank
  sort -n "$1" | awk '{ v[NR] = $1 }
    function at(p,  i) { i = int(p * NR); if (i < p * NR) i++; if (i < 1) i = 1; return v[i] }
    END { if (NR == 0) { print "-"; exit } printf "p50 %s · p75 %s · p90 %s · max %s (n=%d)\n", at(.5), at(.75), at(.9), v[NR], NR }'
}

echo "# Merge queue of ${repo}:main, ${since} to ${until} (UTC)"
echo

# --- 1. Pull requests merged into main -----------------------------------
# One row per pull request merged into main since bases_since: number,
# author, merge date (epoch), path ("queue" when the queue merged it — a
# removal whose reason is `merged` — "direct" otherwise: the admin bypass,
# including a dequeue followed by an admin merge), minutes from first enqueue
# to merge ("-" when direct), merge commit, removals that were not a merge.
q="repo:${repo} is:pr is:merged base:main merged:${bases_since}..${until}"
total="$(gh api graphql -f q="$q" -f query='query($q: String!) { search(query: $q, type: ISSUE, first: 1) { issueCount } }' --jq '.data.search.issueCount')"
if [ "$total" -gt 1000 ]; then
  echo "queue-stats: ${total} merged pull requests in the window; GitHub search stops at 1000 — narrow it" >&2
  exit 1
fi
gh api graphql --paginate -f q="$q" -f query='
query($q: String!, $endCursor: String) {
  search(query: $q, type: ISSUE, first: 50, after: $endCursor) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest {
      number mergedAt author { login } mergeCommit { oid }
      timelineItems(first: 100, itemTypes: [ADDED_TO_MERGE_QUEUE_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT]) {
        nodes { __typename
          ... on AddedToMergeQueueEvent { createdAt }
          ... on RemovedFromMergeQueueEvent { reason } } } } }
  }
}' --jq '.data.search.nodes[] | select(.number != null)
  | [.timelineItems.nodes[] | select(.__typename == "AddedToMergeQueueEvent") | .createdAt] as $adds
  | [.timelineItems.nodes[] | select(.__typename == "RemovedFromMergeQueueEvent") | (.reason // "" | ascii_downcase)] as $removals
  | ($removals | index("merged") != null) as $queued
  | [ (.number | tostring), (.author.login // "ghost"), (.mergedAt | fromdateiso8601 | tostring),
      (if $queued then "queue" else "direct" end),
      (if $queued and ($adds | length) > 0
       then (((.mergedAt | fromdateiso8601) - ($adds | sort | first | fromdateiso8601)) / 60 | floor | tostring)
       else "-" end),
      (.mergeCommit.oid // "-"),
      ($removals | map(select(. != "merged")) | join(","))
    ] | @tsv' | awk -F'\t' '!seen[$1]++' > "$work/prs-all.tsv"
got="$(wc -l < "$work/prs-all.tsv" | tr -d ' ')"
if [ "$got" -ne "$total" ]; then
  echo "queue-stats: the search announced ${total} merged pull requests and paging returned ${got} distinct ones — rerun" >&2
  exit 1
fi
since_epoch="$(date -u -d "$since" +%s)"
until_epoch="$(date -u -d "$until + 1 day" +%s)"
awk -F'\t' -v s="$since_epoch" -v u="$until_epoch" '$3 >= s && $3 < u' "$work/prs-all.tsv" > "$work/prs.tsv"

merged="$(wc -l < "$work/prs.tsv" | tr -d ' ')"
queued="$(awk -F'\t' '$4 == "queue"' "$work/prs.tsv" | wc -l | tr -d ' ')"
echo "## Pull requests merged into main: ${merged} — ${queued} through the queue, $((merged - queued)) directly"
echo "(a direct merge is a push to main: like a release, it rebuilds every merge group in flight)"
awk -F'\t' '{ n[$2 "\t" $4]++ }
  END { for (x in n) { split(x, p, "\t"); printf "  %-28s %-7s %d\n", p[1], p[2], n[x] } }' "$work/prs.tsv" | sort
echo
echo "Minutes from first enqueue to merge, through the queue:"
for author in $(awk -F'\t' '$4 == "queue" { print $2 }' "$work/prs.tsv" | sort -u); do
  awk -F'\t' -v a="$author" '$4 == "queue" && $2 == a { print $5 }' "$work/prs.tsv" > "$work/lat"
  printf '  %-28s %s\n' "$author" "$(pct "$work/lat")"
done
awk -F'\t' '$4 == "queue" { print $5 }' "$work/prs.tsv" > "$work/lat"
printf '  %-28s %s\n' "(all)" "$(pct "$work/lat")"
echo "Removals from the queue that were not a merge, among these pull requests (reason: count):"
awk -F'\t' '$7 != "" { n = split($7, r, ","); for (i = 1; i <= n; i++) c[r[i]]++ } END { for (k in c) printf "  %-28s %d\n", k, c[k] }' "$work/prs.tsv" | sort
echo

# --- 2. Commits on main, by kind -----------------------------------------
gh api --paginate "repos/${repo}/commits?sha=main&since=${bases_since}T00:00:00Z&until=${until}T23:59:59Z&per_page=100" \
  --jq '.[] | [.sha, (.commit.committer.date | fromdateiso8601 | tostring), (.commit.message | split("\n")[0])] | @tsv' \
  | awk -F'\t' '!seen[$1]++' > "$work/commits.tsv"
# sha, epoch, kind: a release, the merge commit of a pull request merged
# through the queue or directly, or any other push.
awk -F'\t' 'FILENAME == ARGV[1] { if ($6 != "-") via[$6] = ($4 == "queue") ? "queue-merge" : "direct-merge"; next }
  { k = ($3 ~ /^chore: release v/) ? "release" : (($1 in via) ? via[$1] : "other-push"); print $1 "\t" $2 "\t" k }' \
  "$work/prs-all.tsv" "$work/commits.tsv" > "$work/kinds.tsv"
awk -F'\t' -v s="$since_epoch" '$2 >= s { c[$3]++ }
  END { printf "## Commits on main: %d merged by the queue, %d merged directly, %d releases, %d other pushes\n(by commit date: a queue merge is dated when its entry was queued, so it can fall on the day before its merge)\n\n", c["queue-merge"], c["direct-merge"], c["release"], c["other-push"] }' "$work/kinds.tsv"

# --- 3. Queue builds, and why they were rebuilt --------------------------
# Day by day: the runs API answers at most 1000 runs per query, and paging a
# large answer has returned the same run twice and skipped others. Each day is
# deduplicated and checked against the count the API announces. The day before
# the window is read too, only to find the build a rebuild replaced when the
# two straddle midnight; counts cover the window alone.
: > "$work/runs.tsv"
day="$(date -u -d "$since - 1 day" +%F)"
while :; do
  url="repos/${repo}/actions/workflows/tests.yml/runs?event=merge_group&created=${day}&per_page=100"
  want="$(gh api "${url%&per_page=100}&per_page=1" --jq '.total_count')"
  gh api --paginate "$url" \
    --jq '.workflow_runs[] | [(.id | tostring), (.created_at | fromdateiso8601 | tostring), (.updated_at | fromdateiso8601 | tostring), .head_branch, .status, (.conclusion // "-")] | @tsv' \
    | awk -F'\t' '!seen[$1]++' > "$work/day.tsv"
  have="$(wc -l < "$work/day.tsv" | tr -d ' ')"
  if [ "$have" -ne "$want" ]; then
    echo "queue-stats: ${day}: the API announced ${want} merge_group runs and paging returned ${have} distinct ones — rerun" >&2
    exit 1
  fi
  cat "$work/day.tsv" >> "$work/runs.tsv"
  [ "$day" != "$until" ] || break
  day="$(date -u -d "$day + 1 day" +%F)"
done

# builds.tsv: created, pull request, base kind (off main: the group of the
# entry ahead, "queue-chain"), outcome, run id, updated, status, and whether
# the build was created inside the window (1) or on the day before it (0).
awk -F'\t' -v s="$since_epoch" 'FILENAME == ARGV[1] { kind[$1] = $3; next }
  { if (match($4, /pr-[0-9]+-[0-9a-f]+$/) == 0) next
    split(substr($4, RSTART + 3), p, "-")
    b = (p[2] in kind) ? kind[p[2]] : "queue-chain"
    out = ($5 == "completed") ? $6 : $5
    print $2 "\t" p[1] "\t" b "\t" out "\t" $1 "\t" $3 "\t" $5 "\t" ($2 >= s ? 1 : 0) }' "$work/kinds.tsv" "$work/runs.tsv" | sort -n > "$work/builds.tsv"
awk -F'\t' '$8 == 1' "$work/builds.tsv" > "$work/window.tsv"

builds="$(wc -l < "$work/window.tsv" | tr -d ' ')"
prs="$(cut -f2 "$work/window.tsv" | sort -u | wc -l | tr -d ' ')"
echo "## Queue builds (Tests, merge_group): ${builds} for ${prs} pull requests"
cut -f4 "$work/window.tsv" | sort | uniq -c | awk '{ printf "  %-28s %d\n", $2, $1 }'
echo
# A rebuild of a pull request replaces its previous build. Both facts are
# read from the clock, not from the order of the rows. The CAUSE: a direct
# push to main committed at most 90 s before the rebuild — GitHub rebuilds
# every group in flight, the entries behind the head included, some twenty
# seconds after such a push — unless the replaced build had already finished
# red before it (that build had ejected its pull request: the rebuild is a
# re-enqueue). Otherwise the queue itself: an entry ahead failed or left, the
# pull request was dequeued and queued again (one dequeued and queued again
# within 90 s of an unrelated push is blamed on the push: none in 2026-09).
# Commit dates stand in for push times: a push lands seconds after its commit,
# but a local series pushed long after it was made would fall outside the
# window. The STATE of the replaced build
# is read at the push for a push-caused rebuild (a build finishing in the
# twenty seconds before the rebuild was still running when the push hit), at
# the rebuild otherwise: green, red, cancelled — or still running, since the
# queue does not cancel the run of a group it discards.
awk -F'\t' 'FILENAME == ARGV[1] { if ($3 != "queue-merge") { n++; t[n] = $2; k[n] = $3 } next }
  {
    if (($2 in pcreated) && $8 == 1) {
      cause = "queue"; best = -1
      for (i = 1; i <= n; i++) if (t[i] > pcreated[$2] && t[i] <= $1 && t[i] >= $1 - 90 && t[i] > best) { best = t[i]; cause = k[i] }
      done = (pstatus[$2] == "completed")
      if (cause != "queue" && done && pupdated[$2] <= best && pout[$2] != "success") cause = "queue"
      at = (cause == "queue") ? $1 : best
      state = (done && pupdated[$2] <= at) ? pout[$2] : "in-flight"
      r[state " -> " cause]++
      if (cause != "queue") {
        lost++; bykind[cause]++
        if (state == "success") green++; else running++
      }
    }
    pcreated[$2] = $1; pupdated[$2] = $6; pstatus[$2] = $7; pout[$2] = $4
  }
  END {
    print "Rebuilds — state of the replaced build -> cause:"
    for (x in r) printf "  %-44s %d\n", x, r[x]
    printf "Queue builds discarded by a direct push to main: %d (already green %d, still running %d); pushes: release %d, direct merge %d, other %d\n",
      lost, green, running, bykind["release"], bykind["direct-merge"], bykind["other-push"]
  }' <(awk -F'\t' '{ print $1 "\t" $2 "\t" $3 }' "$work/kinds.tsv") "$work/builds.tsv" > "$work/rebuilds.txt"
head -1 "$work/rebuilds.txt"
awk 'NR > 1 && !/^Queue builds discarded/' "$work/rebuilds.txt" | sort
awk '/^Queue builds discarded/' "$work/rebuilds.txt"
echo

# --- 4. What failed the failed builds ------------------------------------
echo "## Failed queue builds (named after the pull request at the tail of the group — a failure an entry ahead caused repeats in every group behind it)"
awk -F'\t' '$4 == "failure" { print $5 "\t" $2 }' "$work/window.tsv" > "$work/failed.tsv"
if [ ! -s "$work/failed.tsv" ]; then
  echo "  none"
  exit 0
fi
: > "$work/fails.tsv"
while IFS=$'\t' read -r run pr; do
  gh api "repos/${repo}/actions/runs/${run}/jobs?per_page=100" \
    --jq '.jobs[] | select(.conclusion == "failure") | [(.id | tostring), .name, ([.steps[] | select(.conclusion == "failure") | .name] | join(" + "))] | @tsv' > "$work/jobs.tsv"
  printf '  #%s run %s: %s\n' "$pr" "$run" "$(awk -F'\t' '{ printf "%s%s (%s)", sep, $2, $3; sep = ", " }' "$work/jobs.tsv")"
  [ "$logs" = 1 ] || continue
  while IFS=$'\t' read -r job _ _; do
    gh api "${log_flags[@]}" "repos/${repo}/actions/jobs/${job}/logs" > "$work/log"
    sed -E 's/\x1b\[[0-9;]*m//g' "$work/log" > "$work/log.txt"
    # grep exits 1 on no match — a job that failed outside a test; 2 is an error.
    status=0
    grep -oE -- '--- FAIL: [A-Za-z0-9_]+' "$work/log.txt" > "$work/hits" || status=$?
    [ "$status" -le 1 ] || { echo "queue-stats: grep failed on job ${job}'s log" >&2; exit 1; }
    sed 's/^--- FAIL: //' "$work/hits" | sort -u | sed "s/^/${run}\t/" >> "$work/fails.tsv"
  done < "$work/jobs.tsv"
done < "$work/failed.tsv"
if [ "$logs" = 1 ]; then
  echo
  echo "Failing tests (number of failed builds naming each):"
  sort -u "$work/fails.tsv" | cut -f2 | sort | uniq -c | sort -rn | awk '{ printf "  %-70s %d\n", $2, $1 }'
fi
