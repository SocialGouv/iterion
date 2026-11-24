#!/usr/bin/env bash
# The ARC canary's verdict on the previous probe: green, red, or nothing to
# judge. Driven by .github/workflows/arc-canary.yml, which reads the facts from
# the Actions API and hands them over as environment variables.
#
# Why it is a file: the verdict decides whether iterion pages on its own scale
# set going deaf (#983), and as inline workflow bash nothing in the tree could
# redden on it — not the threshold, not the exemption that keeps a cancelled
# probe from alerting. internal/ciguard/arc_canary_test.go runs THIS script
# over crafted probes.
#
# Environment — every value as the Actions API answers it:
#   PREV_STATUS      the previous probe run's status: queued, in_progress or
#                    completed. Empty when there is no previous probe.
#   PREV_CREATED     its created_at (ISO 8601), needed when it is not completed.
#   JOB_CONCLUSION   the `canary` job's conclusion when the run completed:
#                    success, failure, cancelled, skipped… Empty when the run
#                    carries no such job.
#   JOB_CREATED      the job's created_at (ISO 8601) — when it was dispatched.
#   JOB_STARTED      its started_at (ISO 8601) — when a runner took it. Empty
#                    or "null" for a job no runner ever took.
#   NOW              optional clock override (anything GNU date parses), so the
#                    judgement is reproducible. Defaults to the current time.
#
# Output on stdout, one key per line, for the caller to read:
#   verdict=green|red|skip
#   detail=<why it is red, empty otherwise>
# Everything else goes to stderr: it belongs in the job log, not in the
# caller's parsing.
#
# Exit code: 0 with a verdict, or 2 when the facts cannot support one. A
# missing date is refused rather than read as epoch 0 — a watchdog that judges
# on a guess is worse than one that fails loudly, because its silence reads as
# health.
set -euo pipefail

# STALL_SECONDS is the threshold for both stalls this watchdog can see: a probe
# nothing picked up, and a probe a runner took too late. 25 minutes sits above
# the observed contention p90 for runner wait (~19 minutes on the busiest
# bursts) so ordinary load does not page, and below the ghost-registration time
# scale (tens of minutes to hours) so poisoning does.
STALL_SECONDS=1500

say() { printf '%s\n' "$*" >&2; }

# epoch_of <name> <value> — the value as epoch seconds, or a refusal naming
# which fact is missing.
epoch_of() {
	local name="$1" value="$2" epoch
	if [ -z "$value" ] || [ "$value" = "null" ]; then
		say "arc-canary-verdict: $name is empty — the verdict it feeds cannot be judged on a guess"
		exit 2
	fi
	if ! epoch="$(date -u -d "$value" +%s 2>/dev/null)"; then
		say "arc-canary-verdict: $name is not a date GNU date reads: '$value'"
		exit 2
	fi
	printf '%s' "$epoch"
}

verdict=green
detail=""

prev_status="${PREV_STATUS:-}"
job_conclusion="${JOB_CONCLUSION:-}"
job_started="${JOB_STARTED:-}"

if [ -z "$prev_status" ]; then
	# Day one, or a workflow whose history was cleared.
	say "no previous probe yet — nothing to judge"
	printf 'verdict=skip\ndetail=no previous probe to judge\n'
	exit 0
fi

if [ "$prev_status" != "completed" ]; then
	now="$(epoch_of NOW "${NOW:-$(date -u +%FT%TZ)}")"
	created="$(epoch_of PREV_CREATED "${PREV_CREATED:-}")"
	age=$((now - created))
	say "previous probe still $prev_status after ${age}s"
	if [ "$age" -gt "$STALL_SECONDS" ]; then
		verdict=red
		detail="still '$prev_status' ${age}s after creation — nothing picked it up"
	fi
elif [ -z "$job_conclusion" ]; then
	# The run exists but carries no `canary` job: a cancelled dispatch before
	# the job was created, or a workflow edited between two periods.
	say "previous probe carries no canary job — nothing to judge"
	printf 'verdict=skip\ndetail=the previous probe carries no canary job\n'
	exit 0
elif [ "$job_conclusion" = "cancelled" ]; then
	# A human cancelling a probe — a doubled cron, an incident being worked by
	# hand — is not a stall signal. Judged before any date is read: a job
	# nobody ever started has no started_at, and reading one would turn this
	# exemption into a failed watchdog.
	say "probe job: conclusion=cancelled — a cancelled probe is not a stall signal"
elif [ "$job_conclusion" = "success" ]; then
	started="$(epoch_of JOB_STARTED "$job_started")"
	created="$(epoch_of JOB_CREATED "${JOB_CREATED:-}")"
	wait=$((started - created))
	say "probe job: conclusion=success wait=${wait}s"
	if [ "$wait" -gt "$STALL_SECONDS" ]; then
		verdict=red
		detail="queued ${wait}s before a runner took it"
	fi
else
	# Ran and reported anything else: failure, skipped, timed_out… The wait is
	# part of the message, not of the decision, so a job no runner ever took
	# still gets its verdict instead of killing the watchdog.
	if [ -z "$job_started" ] || [ "$job_started" = "null" ]; then
		waited="an unknown time"
	else
		started="$(epoch_of JOB_STARTED "$job_started")"
		created="$(epoch_of JOB_CREATED "${JOB_CREATED:-}")"
		waited="$((started - created))s"
	fi
	say "probe job: conclusion=$job_conclusion wait=${waited}"
	verdict=red
	detail="ran and reported '$job_conclusion' after ${waited} in queue"
fi

printf 'verdict=%s\ndetail=%s\n' "$verdict" "$detail"
