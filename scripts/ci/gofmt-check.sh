#!/usr/bin/env bash
# gofmt-check.sh — the local member of the `gofmt (check)` step the `test-unit`
# job runs.
#
# WHY IT IS A MEMBER AT ALL. `test-unit` feeds the REQUIRED `test` aggregator,
# so a tracked Go file that drifted out of gofmt's form reddens a required
# context. `task fmt` is the wrong member for it: it runs `go fmt ./...`, which
# REWRITES the source — repairing the defect CI fails on and then reporting
# green is worse than no member at all. This reports and never writes.
#
# WHY IT IS NOT BYTE-IDENTICAL TO CI, deliberately. The job derives its list
# with `find . -name '*.go' -not -path './vendor/*'`, which is right in a CI
# checkout — nothing else is in it — and wrong in an operator's, because
# `find` has no notion of a tracked file: measured 2026-11-12 in
# /home/jo/lab/ai/iterion, it descends into `.works/` (84363 `.go` files),
# `.repos/` (8575) and every worktree registered under `.claude/worktrees/` —
# 164 s and 2124900 files on a warm cache, past the 120 s where a first
# measurement gave up. A member that takes minutes and then names a sibling
# branch's file is not a member. `git ls-files` is the same 6539 files in a
# clean worktree — both forms measured, identical to the FILE, not just to the
# count — and only the tracked ones anywhere else.
#
# THE LIST IS MATERIALISED ONCE, and that is load-bearing. Counting with one
# `git ls-files` and running gofmt over a second one lets the two disagree —
# an index that moves between them is a state this repository expects
# (parallel sessions in one worktree, a concurrent `task check`) — and GNU
# `xargs -0` on EMPTY input still invokes its command once with no argument,
# where `gofmt -l` reads an already-closed stdin, prints nothing and exits 0.
# The two together announce a clean tree over a checkout nothing read. So:
# one file, counted and consumed, and `-0r` so an empty list cannot be green.
#
#   exit 0  every tracked Go file present in the worktree is gofmt-clean
#   exit 1  a file needs gofmt, or a Go file cannot be parsed
#   exit 2  a precondition refused (no gofmt, an empty list) — not a verdict
set -euo pipefail

say() { printf '%s\n' "$*" >&2; }

cd "$(git rev-parse --show-toplevel)"

command -v gofmt > /dev/null 2>&1 || {
	say "ci:gofmt: gofmt is not on PATH. It ships with the Go toolchain devbox pins — run this"
	say "through devbox. Refusing is not a verdict."
	exit 2
}

all=$(mktemp)
list=$(mktemp)
gone=$(mktemp)
out=$(mktemp)
err=$(mktemp)
trap 'rm -f "$all" "$list" "$gone" "$out" "$err"' EXIT

git ls-files -z -- '*.go' ':(exclude)vendor/*' > "$all"

# A tracked path whose file is gone from the worktree — a deletion nobody
# staged yet, which a refactor in flight produces — makes gofmt exit nonzero
# with `lstat: no such file or directory`. Reported as "a Go file it cannot
# parse" that would be a FALSE RED on a healthy tree, and the operator would
# go looking for a syntax error that does not exist. They are set aside and
# NAMED: what this member did not judge is said, never skipped in silence.
: > "$list"
: > "$gone"
while IFS= read -r -d '' f; do
	# `-L` as well as `-e`: `-e` FOLLOWS the link, so a tracked `.go` that is a
	# dead symlink would be filed as a deletion and silently skipped, while
	# CI's `find` lists it and gofmt fails on it. It is judged here too, and
	# gofmt's own error is what the operator reads.
	if [ -e "$f" ] || [ -L "$f" ]; then
		printf '%s\0' "$f" >> "$list"
	else
		printf '%s\0' "$f" >> "$gone"
	fi
done < "$all"

# NUL-counted, both lists: `wc -l` over paths would count the LINES of a file
# name that contains a newline — legal in git — and report a number that is
# not a count of files.
count=$(tr -cd '\0' < "$list" | wc -c)
absent=$(tr -cd '\0' < "$gone" | wc -c)

if [ "$absent" -gt 0 ]; then
	say "ci:gofmt: $absent tracked path(s) absent from the worktree, NOT judged here"
	say "(an unstaged deletion; CI judges the commit, where they exist):"
	tr '\0' '\n' < "$gone" | sed 's/^/  /' >&2
fi

if [ "$count" -eq 0 ]; then
	say "ci:gofmt: no tracked .go file to judge outside vendor/ — in this repository that is a"
	say "broken invocation (wrong directory, a pathspec git did not understand, or every tracked"
	say "file absent from the worktree), not a clean tree."
	exit 2
fi

# `gofmt -l` PRINTS the files that need formatting and exits 0, so the verdict
# is the output, not the status — and the status still matters separately: a
# file gofmt cannot parse makes it exit nonzero, which xargs reports as 123.
# Both are red, for different reasons, and both are told below — including the
# list `gofmt` had already collected before it failed, which is the second
# defect the operator would otherwise discover only on the next run.
set +e
xargs -0r -a "$list" gofmt -l > "$out" 2> "$err"
rc=$?
set -e

if [ "$rc" -ne 0 ]; then
	say "ci:gofmt: gofmt failed over $count tracked file(s) — a Go file it cannot parse:"
	cat "$err" >&2
	if [ -s "$out" ]; then
		say "and, already collected before it failed, $(wc -l < "$out") file(s) needing gofmt:"
		sed 's/^/  /' "$out" >&2
	fi
	exit 1
fi
if [ -s "$out" ]; then
	say "ci:gofmt: gofmt needed on:"
	sed 's/^/  /' "$out" >&2
	say ""
	say "$(wc -l < "$out") of $count tracked file(s). Run \`task fmt\` (it REWRITES) or gofmt -w on each."
	exit 1
fi

say "ci:gofmt: $count tracked .go file(s) outside vendor/ are gofmt-clean"
