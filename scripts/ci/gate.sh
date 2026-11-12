#!/usr/bin/env bash
# gate.sh — the local gate the operator direct-landing lane asks for.
#
# It runs the members `.github/ci-gate-map.yaml` names, reads each one's exit
# code SEPARATELY, and reports what it ran against what the map says exists.
# `docs/agents/workflow/review-and-merge.md` names `task ci:gate` as the lane's
# first blocking proof; this is that definition.
#
# WHAT IT DOES NOT CLAIM. The map is honest about four dispositions, and this
# runner reports them rather than averaging them away: a gate that answered
# "green" while two required contexts had no member would be the control this
# repository has already paid for twice. So the verdict has three parts —
# the members that ran and their codes, the required contexts with no member,
# and the count. A required context not covered makes the exit code 3, which
# is NOT the same as a red member (1): one says the gate is incomplete, the
# other that the tree is.
#
#   exit 0  every member ran green AND every required context has a member
#   exit 1  a member is red
#   exit 2  a precondition refused (yq missing, the map unreadable) — not a verdict
#   exit 3  members green, but a required context has no member: INCOMPLETE
#
# --list prints the plan and exits 0 without running anything.
set -euo pipefail

say() { printf '%s\n' "$*" >&2; }

cd "$(git rev-parse --show-toplevel)"
MAP=.github/ci-gate-map.yaml
EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-.iterion/ci-gate/$(git rev-parse --short HEAD)}
LIST_ONLY=0
[ "${1:-}" = "--list" ] && LIST_ONLY=1

command -v yq > /dev/null 2>&1 || {
	say "ci:gate: yq is not on PATH. devbox pins yq-go 4.53.3, the same version the workflow"
	say "installs by digest — run this through devbox. Refusing is not a verdict."
	exit 2
}
[ -f "$MAP" ] || {
	say "ci:gate: $MAP is missing. The gate's definition is that file; without it there is"
	say "nothing to run and nothing to report. Refusing is not a verdict."
	exit 2
}

# ---------------------------------------------------------------------------
# The plan, read from the map. Mutating members LAST.
# ---------------------------------------------------------------------------
# Deduplicated: several jobs legitimately name the same member (the aggregators'
# legs share `task test`), and running one twice would double the wall clock
# and make `ran N of N` a different number from the plan.
mapfile -t PLAIN < <(yq -r '
  [.jobs[] | select(.members) | select(.mutates != true) | .members[]] | unique | .[]' "$MAP")
mapfile -t MUTATING < <(yq -r '
  [.jobs[] | select(.members) | select(.mutates == true) | .members[]] | unique | .[]' "$MAP")
# A required context counts as covered when it has members — or, for an
# AGGREGATOR, when every leg it aggregates has them.
#
# `staged` counts too. The lane asks for "the full required-check set a
# workflow job can express", and a context this repository staged ahead of the
# ruleset is one it has already decided will block a merge. Counting only
# `yes` would let the gate call itself complete over a check that is about to
# refuse merges — and `fmt-check` is exactly that case today: expressible,
# staged, and unwritten.
#
# The second clause is why this is not one `select`. "Required but
# unexpressible, so not my problem" would be a loophole wide enough to drive
# the whole gate through: `test` and `race` are required and are pure
# aggregators, so excluding them unconditionally would let a gate call itself
# complete while test-unit had no member at all. The exclusion has to be
# EARNED by the legs, and it is checked here rather than asserted in a comment.
mapfile -t UNCOVERED < <(yq -r '
  .jobs as $j
  | [ $j | to_entries[]
      | select(.value.required == "yes" or .value.required == "staged")
      | select(
          (.value.members == null)
          and (
            (.value.aggregates == null)
            or ([.value.aggregates[] | select(($j[.].members // null) == null)] | length > 0)
          )
        )
      | .key ] | unique | .[]' "$MAP")

MEMBERS=("${PLAIN[@]}" "${MUTATING[@]}")
total=${#MEMBERS[@]}

say "ci:gate: $total member(s) from $MAP (${#MUTATING[@]} mutating, run last)"
if [ "${#UNCOVERED[@]}" -gt 0 ]; then
	say "ci:gate: INCOMPLETE — required context(s) with no member: ${UNCOVERED[*]}"
fi
if [ "$LIST_ONLY" = "1" ]; then
	printf 'plan:\n'
	for m in "${PLAIN[@]}"; do printf '  task %s\n' "$m"; done
	for m in "${MUTATING[@]}"; do printf '  task %s   (mutates, last)\n' "$m"; done
	printf 'uncovered required: %s\n' "${UNCOVERED[*]:-none}"
	exit 0
fi

mkdir -p "$EVIDENCE"
MATRIX="$EVIDENCE/matrix.tsv"
: > "$MATRIX"

# The posture the evidence has to carry, so a landing record says what machine
# produced the verdict. push-to-main.md asks for exactly this.
{
	printf 'head\t%s\n' "$(git rev-parse HEAD)"
	printf 'goversion\t%s\n' "$(go env GOVERSION 2> /dev/null || echo unknown)"
	printf 'goflags\t%s\n' "$(go env GOFLAGS 2> /dev/null || echo unknown)"
	printf 'cgo\t%s\n' "$(go env CGO_ENABLED 2> /dev/null || echo unknown)"
	printf 'dirty\t%s\n' "$(git status --porcelain | wc -l)"
} > "$EVIDENCE/posture.tsv"

ran=0
failed=0
for m in "${MEMBERS[@]}"; do
	ran=$((ran + 1))
	log="$EVIDENCE/${m//[:\/]/_}.log"
	say "ci:gate: [$ran/$total] task $m"
	# Redirected to a FILE, the code read on the very next line. Never a pipe
	# on a gate command: `| head` closes it, SIGPIPE kills the run and the
	# status read is head's — this repository announced a green Go suite over a
	# red package that way.
	set +e
	task "$m" > "$log" 2>&1
	rc=$?
	set -e
	printf '%s\t%d\t%s\n' "$m" "$rc" "$log" >> "$MATRIX"
	if [ "$rc" -ne 0 ]; then
		failed=1
		say "ci:gate: member $m FAILED (rc=$rc) — $log"
		tail -20 "$log" >&2 || true
	fi
done

say ""
say "ci:gate: ran $ran of $total member(s)"
cat "$MATRIX" >&2
say "evidence: $EVIDENCE"

# The count is compared, not trusted. "Running two of three members proves
# nothing about the third" is answered by a number, never by a chain of &&.
if [ "$ran" -ne "$total" ]; then
	say "ci:gate: ran $ran of $total — the runner did not finish its own plan."
	exit 2
fi
if [ "$failed" -ne 0 ]; then
	exit 1
fi
if [ "${#UNCOVERED[@]}" -gt 0 ]; then
	say "ci:gate: every member is green, but ${#UNCOVERED[@]} required context(s) have no member"
	say "(${UNCOVERED[*]}). The gate is INCOMPLETE: this is not the lane's first proof yet."
	exit 3
fi
say "ci:gate: green, and every required context has a member"
