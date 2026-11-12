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
#   exit 0  every member ran green AND every required or staged context is
#           covered — by a member of its own, or, for a row that AGGREGATES
#           (any row with `aggregates:`, not only a pure one), by the members
#           of every leg it names
#   exit 1  a member is red
#   exit 2  nothing was judged: a precondition refused — this runner's own (yq
#           missing, the map unreadable or unusable) or a MEMBER's
#           (`refusing_members`) — or the invocation was wrong (an unknown
#           argument), or the runner did not finish its own plan. A refusal is
#           not a verdict
#   exit 3  members green, but a required or staged context has no member at
#           all: INCOMPLETE
#
# Exit 0 also NAMES every REACHED row whose disposition is `partial` — a
# required or staged context, or a leg a required aggregator speaks for.
# A member is not an equivalence: those jobs hold a residual this runner does
# not execute, and a verdict that hid them would read as a claim about CI.
#
# --list prints the plan and exits 0 without running anything.
set -euo pipefail

say() { printf '%s\n' "$*" >&2; }

cd "$(git rev-parse --show-toplevel)"
MAP=.github/ci-gate-map.yaml
EVIDENCE=${ITERION_CI_GATE_EVIDENCE:-.iterion/ci-gate/$(git rev-parse --short HEAD)}
LIST_ONLY=0
case "${1:-}" in
	"") ;;
	--list) LIST_ONLY=1 ;;
	*)
		# Not cosmetic: an argument this runner does not know used to fall
		# through to a FULL run — including the mutating member, which rewrites
		# go.mod, go.sum and vendor/. `gate.sh --dry-run` must refuse, not
		# rewrite the tree.
		say "ci:gate: unknown argument $(printf %q "$1"). The only one is --list, which prints the"
		say "plan and runs nothing. Refusing is not a verdict."
		exit 2
		;;
esac
[ "$#" -le 1 ] || {
	say "ci:gate: $# arguments; this runner takes at most one (--list). Refusing is not a verdict."
	exit 2
}

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
# The map PARSES into what the queries below assume. Load-bearing, and the
# one thing a `mapfile < <(yq …)` cannot tell you: a process substitution
# hides yq's exit code from `set -e` and from `pipefail` alike, so an empty
# array is indistinguishable from "nothing to report". Measured on this
# runner before these two gates existed: an empty map, a map whose top level
# was renamed, and a map with ONE missing pair of brackets each produced
# `ran 0 of 0` followed by "green, and every required context has a member",
# exit 0 — the last of them in complete silence. The Go reader of this same
# file has the floor (TestTheCIGateMapIsTotalAndHonest fatals on an empty
# `jobs`); the runner owed it too.
yq -e '.jobs | (type == "!!map") and (length > 0)' "$MAP" > /dev/null 2>&1 || {
	say "ci:gate: $MAP does not parse into a non-empty \`jobs\` map — unreadable, empty, or its"
	say "top-level key is not \`jobs\`. Every query below would answer nothing, which is not the"
	say "same as nothing to report. Refusing is not a verdict."
	exit 2
}
# `aggregates: test-unit` instead of `aggregates: [test-unit]` is the quiet
# one: the splat `.value.aggregates[]` over a STRING yields nothing, so the
# coverage query drops the row and a required context with no member stops
# being reported. One character between "INCOMPLETE, exit 3" and "green,
# exit 0".
yq -e '[ .jobs[] | select(has("aggregates")) | .aggregates | select(type != "!!seq" or length == 0) ] | length == 0' \
	"$MAP" > /dev/null 2>&1 || {
	say "ci:gate: a row's \`aggregates:\` is not a NON-EMPTY sequence in $MAP (a missing pair of"
	say "brackets, or \`[]\`). The coverage query splats it, and over a scalar or an empty sequence"
	say "a splat yields NOTHING — the row drops out of the uncovered set in silence, which is the"
	say "same false green either way. Refusing is not a verdict."
	exit 2
}
# `members:` is the same hole, and worse: the plan query splats it, so the
# member VANISHES from the run, while the coverage query reads
# `.members == null` as false and counts the row COVERED. A required context
# whose member never ran, reported green and never named.
yq -e '[ .jobs[] | select(has("members")) | .members | select(type != "!!seq" or length == 0) ] | length == 0' \
	"$MAP" > /dev/null 2>&1 || {
	say "ci:gate: a row's \`members:\` is not a NON-EMPTY sequence in $MAP (a missing pair of"
	say "brackets, or \`[]\`). The plan query splats it — and in yq an empty sequence is falsy, so"
	say "\`select(.members)\` drops the row too — while the coverage query reads \`[] == null\` as"
	say "false and counts the row COVERED. Omit the key to mean \"no member\". Refusing is not a verdict."
	exit 2
}
# And every ENTRY is a non-empty string, which is the same rule one level
# down. yq renders a `null` entry as the four characters `null` and a nested
# sequence as its JSON text, so the runner would invoke `task null` or
# `task ["first"]`: a member the author never named, failing for a reason
# nobody wrote. That is a false RED, and in a tool that blocks a landing a
# false red costs what a hole costs.
yq -e '[ .jobs[] | select(has("members")) | .members[] | select(type != "!!str" or . == "") ] | length == 0' \
	"$MAP" > /dev/null 2>&1 || {
	say "ci:gate: a \`members:\` entry in $MAP is not a non-empty string (a null, a nested list, a"
	say "number). The plan would invoke it verbatim — \`task null\` — and fail for a reason nobody"
	say "wrote. Refusing is not a verdict."
	exit 2
}
# There is deliberately NO entry-level check on `aggregates:`, and the
# asymmetry is measured rather than assumed: the plan query SPLATS and then
# INVOKES a member, so a `null` entry becomes `task null`; the coverage query
# only LOOKS UP (`$j[.]`), and a non-string key resolves to null, which leaves
# the row in the uncovered set — the safe direction, exit 3. A refusal there
# would buy nothing and could refuse a map this runner handles correctly.
#
# `refusing_members` is the third list these queries splat, and it needs the
# same shape rule as `members`: a scalar makes the query fail inside a process
# substitution, the list comes back EMPTY, and every member that refuses is
# then read as RED — the exact inversion the list exists to prevent.
# Two expressions rather than one: yq's `and` does not compose a type test
# with a list reduction (measured — it answers `false` and `no matches found`
# on a map that is in fact correct, which would have been a refusal on every
# tree). An ABSENT key passes both: no refusing member is a legal map.
# Keyed on `has(…)` rather than on `// []`, which treats `false` as absent and
# would let `refusing_members: false` through — and an empty sequence is
# refused for the same reason its siblings are: both leave the list empty, and
# an empty list reads every refusal as RED. An ABSENT key stays legal: a map
# with no refusing member is a map, and it is the `has` that says so.
yq -e '[ select(has("refusing_members")) | .refusing_members | select(type != "!!seq" or length == 0) ] | length == 0' \
	"$MAP" > /dev/null 2>&1 || {
	say "ci:gate: \`refusing_members\` in $MAP is present but is not a non-empty sequence. The query"
	say "that reads it would come back empty, and every member that REFUSES would then be reported"
	say "red — a machine that cannot judge, read as a broken repository. Omit the key to declare"
	say "none. Refusing is not a verdict."
	exit 2
}
# `test("^\\S+$")` and not merely non-empty: a trailing space makes the entry
# match no member, so every refusal that member reports would be read as RED —
# the inversion again, from one invisible character.
yq -e '[ (.refusing_members // [])[] | select(type != "!!str" or (test("^\S+$") | not)) ] | length == 0' \
	"$MAP" > /dev/null 2>&1 || {
	say "ci:gate: a \`refusing_members\` entry in $MAP is not a bare non-empty word (an empty string,"
	say "or one with whitespace in it), so no member can ever match it and its refusals would read"
	say "as red. Refusing is not a verdict."
	exit 2
}

# ---------------------------------------------------------------------------
# The plan, read from the map. Mutating members LAST.
# ---------------------------------------------------------------------------
# Deduplicated, because two rows naming the same member is legitimate — a
# member that reproduces part of two jobs — and running it twice would double
# the wall clock and make `ran N of N` a different number from the plan. No
# row shares one today (measured: `group_by` over every member returns no
# group of two), so this is a rule about what the map MAY say, not a
# description of it; internal/ciguard's guarantee test compacts both sides for
# exactly this reason.
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

# A member is not an equivalence. `disposition: partial` means the member
# covers PART of its job, and the row's `residual` says what is left — so a
# required context can be SATISFIED by a member and still hold a class of
# defect this runner cannot see (CI's `CI=true`, its `--frozen-lockfile`, its
# sharding). Exit 0 says "green and every required context has a member";
# printing these is what stops that from being read as "CI can find nothing
# the gate missed".
#
# They are named for what they ARE: four of the five are `required: no` legs
# that an aggregator speaks for, not required contexts — `requiredChecks` does
# not hold them, and calling them required here would be exactly the kind of
# prose this file exists to refuse.
#
# Computed as an intersection in shell rather than in one yq expression: the
# reachable set has to follow `aggregates`, and a membership test folded into
# the filter is where this file's first version silently matched everything.
mapfile -t REACHED < <(yq -r '
  [ .jobs | to_entries[]
    | select(.value.required == "yes" or .value.required == "staged")
    | [.key] + (.value.aggregates // []) ] | flatten | unique | .[]' "$MAP")
mapfile -t PARTIAL < <(yq -r '
  [ .jobs | to_entries[] | select(.value.disposition == "partial") | .key ] | unique | .[]' "$MAP")
CAVEATS=()
for p in "${PARTIAL[@]}"; do
	for r in "${REACHED[@]}"; do
		[ "$p" = "$r" ] && CAVEATS+=("$p") && break
	done
done

MEMBERS=("${PLAIN[@]}" "${MUTATING[@]}")
total=${#MEMBERS[@]}

say "ci:gate: $total member(s) from $MAP (${#MUTATING[@]} mutating, run last)"
if [ "$total" -eq 0 ]; then
	say "ci:gate: the map parsed and yielded NO member. In this repository that is a broken read —"
	say "a shape the preconditions above do not name — not a tree with nothing to check."
	say "Refusing is not a verdict."
	exit 2
fi
if [ "${#UNCOVERED[@]}" -gt 0 ]; then
	say "ci:gate: INCOMPLETE — required/staged context(s) with no member: ${UNCOVERED[*]}"
fi
if [ "${#CAVEATS[@]}" -gt 0 ]; then
	say "ci:gate: PARTIAL coverage BEHIND the required set — job(s) whose member runs only part"
	say "of them: ${CAVEATS[*]}. Each row's \`residual\` in $MAP names what is left to CI."
fi
if [ "$LIST_ONLY" = "1" ]; then
	printf 'plan:\n'
	for m in "${PLAIN[@]}"; do printf '  task %s\n' "$m"; done
	for m in "${MUTATING[@]}"; do printf '  task %s   (mutates, last)\n' "$m"; done
	printf 'uncovered required:  %s\n' "${UNCOVERED[*]:-none}"
	printf 'partial behind set: %s\n' "${CAVEATS[*]:-none}"
	exit 0
fi

# Without this, an unwritable evidence directory made the runner exit 1 — the
# documented code for "a member is red" — having run no member and printed no
# verdict at all.
mkdir -p "$EVIDENCE" || {
	say "ci:gate: cannot create the evidence directory $EVIDENCE. The matrix of exit codes IS the"
	say "verdict, so there is nowhere to write one. Refusing is not a verdict."
	exit 2
}
MATRIX="$EVIDENCE/matrix.tsv"
: > "$MATRIX" || {
	say "ci:gate: cannot write $MATRIX. Refusing is not a verdict."
	exit 2
}

# The posture the evidence has to carry, so a landing record says what machine
# produced the verdict. push-to-main.md asks for exactly this.
{
	printf 'head\t%s\n' "$(git rev-parse HEAD)"
	printf 'goversion\t%s\n' "$(go env GOVERSION 2> /dev/null || echo unknown)"
	printf 'goflags\t%s\n' "$(go env GOFLAGS 2> /dev/null || echo unknown)"
	printf 'cgo\t%s\n' "$(go env CGO_ENABLED 2> /dev/null || echo unknown)"
	printf 'dirty\t%s\n' "$(git status --porcelain | wc -l)"
} > "$EVIDENCE/posture.tsv"

# The members that answer exit 2 for "a precondition refused — not a verdict",
# read from the map so this runner and internal/ciguard share one list. The
# distinction is the whole reason this file has an exit 2 of its own: a member
# that CANNOT run (no docker reachable, a dirty tree, no gofmt) says nothing
# about the tree, and reporting it as `1` — "a member is red" — would make a
# sandboxed session read an unreachable service as a broken repository. It is
# a declared list rather than "any rc == 2" on purpose: plenty of tools spend
# 2 on a real failure, and promoting those to "refused" would be the one
# direction that turns a red into a shrug.
mapfile -t REFUSING < <(yq -r '(.refusing_members // []) | .[]' "$MAP")

step=0
failed=0
REFUSED=()
for m in "${MEMBERS[@]}"; do
	step=$((step + 1))
	log="$EVIDENCE/${m//[:\/]/_}.log"
	say "ci:gate: [$step/$total] task $m"
	# `-x` is LOAD-BEARING, and nothing else in this repository passes it:
	# measured on the pinned go-task 3.48.0, a task whose command exits 2
	# makes `task` exit 201, and so does one that exits 7 — every failure
	# collapses into one number. Without it this runner's whole premise ("each
	# member's exit code, read separately") is false, the matrix records 201
	# for everything, and the `refusing_members` class below is dead code: a
	# machine that cannot judge would read as a broken repository.
	#
	# Redirected to a FILE, the code read on the very next line. Never a pipe
	# on a gate command: `| head` closes it, SIGPIPE kills the run and the
	# status read is head's — this repository announced a green Go suite over a
	# red package that way.
	set +e
	task -x "$m" > "$log" 2>&1
	rc=$?
	set -e
	printf '%s\t%d\t%s\n' "$m" "$rc" "$log" >> "$MATRIX"
	declares_refusal=0
	for r in "${REFUSING[@]}"; do
		[ "$m" = "$r" ] && declares_refusal=1 && break
	done
	if [ "$rc" -eq 2 ] && [ "$declares_refusal" = "1" ]; then
		REFUSED+=("$m")
		say "ci:gate: member $m REFUSED (rc=2, the precondition it names) — $log"
		tail -20 "$log" >&2 || true
	elif [ "$rc" -ne 0 ]; then
		failed=1
		say "ci:gate: member $m FAILED (rc=$rc) — $log"
		tail -20 "$log" >&2 || true
	fi
done

# Measured on the EVIDENCE, never on the loop counter: incrementing a variable
# at the top of the body counts ITERATIONS, and the two could not diverge. The
# matrix holds one line per member whose exit code was actually recorded.
#
# What this comparison can and cannot catch, said plainly: a loop that ran to
# its end always writes one row per member, so the honest claim is that the
# number comes from the artefact rather than from the script's intention — a
# truncated matrix, a lost append, a signal that killed the loop. The test
# that holds "one row per member of the PLAN" is in internal/ciguard, against
# the map's own member count.
ran=$(wc -l < "$MATRIX")

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
if [ "${#REFUSED[@]}" -gt 0 ]; then
	say "ci:gate: no member is red, but ${#REFUSED[@]} REFUSED to run (${REFUSED[*]}): this machine"
	say "cannot meet a precondition they name, so the gate has NO verdict on their jobs."
	exit 2
fi
if [ "${#UNCOVERED[@]}" -gt 0 ]; then
	say "ci:gate: every member is green, but ${#UNCOVERED[@]} required/staged context(s) have no"
	say "member (${UNCOVERED[*]}). The gate is INCOMPLETE: this is not the lane's first proof yet."
	exit 3
fi
say "ci:gate: green, and every required or staged context is covered"
if [ "${#CAVEATS[@]}" -gt 0 ]; then
	say "ci:gate: with PARTIAL coverage behind ${CAVEATS[*]} — a green verdict here is"
	say "\"every member passed\", never \"CI can find nothing this did not run\"."
fi
