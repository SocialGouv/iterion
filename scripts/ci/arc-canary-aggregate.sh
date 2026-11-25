#!/usr/bin/env bash
# One verdict for the ARC canary out of one verdict per runner pool.
#
# .github/workflows/arc-canary.yml probes every pool (small, large, heavy) and
# runs scripts/ci/arc-canary-verdict.sh once per pool; this file folds those
# answers into the single decision the watchdog acts on (page, resolve, or
# nothing). A fold written inline in the workflow could only be read, never
# exercised: internal/ciguard/arc_canary_test.go runs this one.
#
# Input on stdin, one line per pool: <pool>\t<verdict>\t<detail>
# Output on stdout: verdict=green|red|skip and detail=<the red pools and why>
#
# Rule: any red pool pages (a pool that hands out no job blocks the checks
# routed to it, however healthy the others are); otherwise any green pool
# resolves; a canary with only skips judges nothing. An answer the fold
# cannot place, or no answer at all, exits 2: a watchdog that guesses reads
# as health.
set -euo pipefail

reds=""
greens=0
lines=0
while IFS=$'\t' read -r pool verdict detail || [ -n "${pool:-}" ]; do
	[ -n "$pool" ] || continue
	lines=$((lines + 1))
	case "$verdict" in
	red) reds="${reds:+$reds; }$pool: $detail" ;;
	green) greens=$((greens + 1)) ;;
	skip) ;;
	*)
		printf 'arc-canary-aggregate: pool %s answered %s — not a verdict\n' "$pool" "'$verdict'" >&2
		exit 2
		;;
	esac
done
if [ "$lines" -eq 0 ]; then
	printf 'arc-canary-aggregate: no pool verdict on stdin\n' >&2
	exit 2
fi
if [ -n "$reds" ]; then
	printf 'verdict=red\ndetail=%s\n' "$reds"
elif [ "$greens" -gt 0 ]; then
	printf 'verdict=green\ndetail=\n'
else
	printf 'verdict=skip\ndetail=no pool had a probe to judge\n'
fi
