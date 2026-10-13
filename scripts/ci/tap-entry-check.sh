#!/usr/bin/env bash
# Verifies a Homebrew tap update whose heavy checks tests.yml skipped.
#
# tests.yml skips its heavy required jobs for a pull request or a merge-queue
# entry titled `chore(brew): update tap to …` — the pull request
# brew-update.yml opens, which moves the version and sha256 values of two
# files, values no heavy job's verdict depends on. A job skipped by its `if:`
# reports SUCCESS, so the skip is exactly as honest as this check. It runs in `brand`: required, never skipped, and already a
# reader of both tap files. internal/ciguard pins this step, the predicate and
# its copies together.
#
# Environment:
#   TAP_ONLY      the skip predicate as tests.yml evaluated it: true or false.
#                 Received, never re-derived from the title: GitHub's
#                 startsWith ignores case, and a stricter matcher here would
#                 wave through an entry the skip let pass.
#   EVENT_NAME    github.event_name — merge_group or pull_request.
#   merge_group:  QUEUE_BASE_SHA, QUEUE_HEAD_SHA, QUEUE_BASE_REF, plus
#                 GITHUB_API_URL, GITHUB_REPOSITORY and GH_TOKEN to read the
#                 merge queue's live grouping strategy.
#   pull_request: PR_HEAD_SHA, the head the run tests. The checkout must be
#                 the pull request's test merge with its parents
#                 (actions/checkout's default ref, fetch-depth: 2).
#
# A tap update is what scripts/update-brew-tap.sh writes: the version and
# sha256 values of the two tap files — a dotted numeric version, a 64-hex
# digest — and nothing else. Any other change to those files is not one,
# whatever its title: the heavy checks that read them (internal/ciguard's)
# depend on everything but those values.
#
# Exit 0: not a tap update, or a verified one. Exit 1: everything else — a
# tap title over a diff that reaches past the tap or reshapes it, a queue that
# does not group ALLGREEN, an input this script cannot read. Every refusal
# fails closed: the pull request stays red, the queue entry leaves the queue
# unmerged.
set -euo pipefail

# The tap: the only paths a skipped entry may change, matched exactly.
allowed=(
  Formula/iterion.rb
  Cask/iterion-desktop.rb
)

fail() {
  echo "::error::$*" >&2
  exit 1
}

need() {
  [ -n "${!1:-}" ] || fail "$1 is empty — the brand step must pass it on ${EVENT_NAME:-this event}"
}

case "${TAP_ONLY:-}" in
false)
  echo "not a Homebrew tap update — the heavy checks ran, nothing to verify"
  exit 0
  ;;
true) ;;
*) fail "TAP_ONLY is '${TAP_ONLY:-}', expected true or false: the step must receive the skip predicate as tests.yml evaluates it" ;;
esac

case "${EVENT_NAME:-}" in
merge_group)
  need QUEUE_BASE_SHA
  need QUEUE_HEAD_SHA
  need QUEUE_BASE_REF
  head="$(git rev-parse HEAD)"
  [ "$head" = "$QUEUE_HEAD_SHA" ] ||
    fail "the checkout is ${head}, the queue built ${QUEUE_HEAD_SHA}: this would judge another tree"
  base="$QUEUE_BASE_SHA"
  if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
    git fetch --quiet --no-tags --depth=1 origin "$base" ||
      fail "cannot fetch the entry's base ${base}"
  fi
  ;;
pull_request)
  need PR_HEAD_SHA
  read -r -a commit <<<"$(git rev-list --parents -n 1 HEAD)"
  [ "${#commit[@]}" -eq 3 ] ||
    fail "HEAD ${commit[0]:-?} has $((${#commit[@]} - 1)) parent(s): the verifier judges the pull request's test merge (base, head) and needs it checked out with fetch-depth: 2"
  [ "${commit[2]}" = "$PR_HEAD_SHA" ] ||
    fail "the test merge's second parent is ${commit[2]}, but the run tests head ${PR_HEAD_SHA}"
  head="${commit[0]}"
  base="${commit[1]}"
  ;;
*)
  fail "the tap skip took effect on a '${EVENT_NAME:-}' event — the predicate reads only merge_group and pull_request"
  ;;
esac

# To a file first: a failed diff behind a pipe or a process substitution
# would read as an empty one, and an empty diff verifies.
changes="$(mktemp)"
trap 'rm -f "$changes"' EXIT
git diff -z --name-only --no-renames "$base" "$head" >"$changes" ||
  fail "cannot diff ${base}..${head}"

outside=()
count=0
while IFS= read -r -d '' path; do
  count=$((count + 1))
  inside=false
  for a in "${allowed[@]}"; do
    if [ "$path" = "$a" ]; then
      inside=true
    fi
  done
  if [ "$inside" = false ]; then
    outside+=("$path")
  fi
done <"$changes"

if [ "${#outside[@]}" -gt 0 ]; then
  printf '  %s\n' "${outside[@]}" >&2
  fail "titled as a Homebrew tap update, but it changes ${#outside[@]} path(s) outside the tap (listed above), and the heavy checks were skipped on the strength of that title. Retitle it: anything else runs the whole suite"
fi

# Each tap file before and after: the same mode and type (a regular file
# stays one, no executable bit, no symlink), and contents that differ only in
# version and sha256 values update-brew-tap.sh writes — tap_values_only.py
# compares the bytes line for line and says why each rule is there. It sits
# beside this script, and a diff that touches it never reaches this point:
# it is outside the tap.
values_only="$(dirname "$(readlink -f "$0")")/tap_values_only.py"
before_file="$(mktemp)"
after_file="$(mktemp)"
trap 'rm -f "$changes" "$before_file" "$after_file"' EXIT
reshaped=()
for path in "${allowed[@]}"; do
  before_mode="$(git ls-tree "$base" -- "$path" | cut -d' ' -f1)"
  after_mode="$(git ls-tree "$head" -- "$path" | cut -d' ' -f1)"
  git show "${base}:${path}" >"$before_file" 2>/dev/null || echo "<absent>" >"$before_file"
  git show "${head}:${path}" >"$after_file" 2>/dev/null || echo "<absent>" >"$after_file"
  if [ "$before_mode" != "$after_mode" ] || ! python3 -I "$values_only" "$before_file" "$after_file"; then
    reshaped+=("$path")
  fi
done
if [ "${#reshaped[@]}" -gt 0 ]; then
  fail "titled as a Homebrew tap update, but it changes more than the version and sha256 values of ${reshaped[*]} — a tap update moves those values only. Retitle it: anything else runs the whole suite"
fi

if [ "$EVENT_NAME" = merge_group ]; then
  # Entries stack: this one was built on the entry ahead of it, and its
  # skipped checks vouch for nothing that entry brought. ALLGREEN merges an
  # entry only when its own checks pass; HEADGREEN would merge everything
  # ahead of a green tail, this tail included. So the strategy is read live,
  # where the ruleset keeps it, at the moment of the verdict.
  need GITHUB_API_URL
  need GITHUB_REPOSITORY
  need GH_TOKEN
  branch="${QUEUE_BASE_REF#refs/heads/}"
  rules="$(curl -fsS --retry 3 --retry-delay 1 --retry-all-errors --max-time 30 \
    -H "Authorization: Bearer ${GH_TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "${GITHUB_API_URL}/repos/${GITHUB_REPOSITORY}/rules/branches/${branch}?per_page=100")" ||
    fail "cannot read the rules that apply to ${branch}"
  strategy="$(python3 -I -c '
import json, sys
rules = json.load(sys.stdin)
if not isinstance(rules, list):
    sys.exit("the rules endpoint did not answer a list")
if len(rules) >= 100:
    sys.exit("100 rules or more: one page may not hold them all")
queue = sorted({str((r.get("parameters") or {}).get("grouping_strategy")) for r in rules if r.get("type") == "merge_queue"})
print(",".join(queue))
' <<<"$rules")" || fail "cannot read a merge-queue rule out of the rules of ${branch}"
  [ "$strategy" = ALLGREEN ] ||
    fail "the merge queue on ${branch} groups '${strategy}', not ALLGREEN: a tap entry's skipped checks would vouch for the entries ahead of it. Drop the tap skip from tests.yml, or restore ALLGREEN"
fi

echo "verified Homebrew tap update: ${count} path(s), all in the tap (${EVENT_NAME}${strategy:+, queue groups $strategy})"
