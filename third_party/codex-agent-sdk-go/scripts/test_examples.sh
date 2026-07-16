#!/usr/bin/env bash
#
# test_examples.sh — Run SDK examples and verify output using Codex CLI.
#
# Discovers runnable examples in examples/*, runs each one, then asks Codex
# to judge whether the output matches the source code's intended behavior.
#
# Usage: scripts/test_examples.sh [options]
#   -n PARALLEL   Max examples to run concurrently (default: 5)
#   -t TIMEOUT    Per-example timeout in seconds (default: 120)
#   -o DIR        Output directory for logs (default: /tmp/sdk-example-tests-<timestamp>)
#   -s EXAMPLES   Comma-separated list of examples to skip
#   -f EXAMPLES   Comma-separated list of examples to run (filter, run only these)
#   -m MODEL      Codex model to use for verification (default: CLI default)
#   -k            Keep going on failure (default: stop on first failure in summary)
#   -h            Help
#

set -euo pipefail

# ---------------------------------------------------------------------------
# Signal handling
# ---------------------------------------------------------------------------
INTERRUPTED=false
declare -a WORKER_PIDS=()

cleanup() {
	INTERRUPTED=true
	echo ""
	echo "Interrupted - killing workers..."

	for pid in "${WORKER_PIDS[@]}"; do
		pkill -P "$pid" 2>/dev/null || true
		kill "$pid" 2>/dev/null || true
	done

	# Wait briefly then force-kill stragglers.
	sleep 0.3

	for pid in "${WORKER_PIDS[@]}"; do
		kill -9 "$pid" 2>/dev/null || true
	done
}

trap 'cleanup' INT TERM

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
PARALLEL=5
TIMEOUT=120
OUTDIR=""
SKIP_LIST=""
FILTER_LIST=""
VERIFY_MODEL=""
KEEP_GOING=false

# Directories in examples/ that are not runnable Go examples.
SKIP_DIRS="plugins"

# Upper bound on output log content embedded in verifier prompt.
VERIFY_MAX_CHARS=24000

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
usage() {
	sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# //'
	exit 0
}

die() {
	echo "ERROR: $*" >&2
	exit 1
}

# Check if a value exists in a comma-separated list.
in_list() {
	local needle="$1"
	local haystack="$2"

	[[ -z "$haystack" ]] && return 1

	IFS=',' read -ra items <<<"$haystack"

	for item in "${items[@]}"; do
		[[ "$item" == "$needle" ]] && return 0
	done

	return 1
}

# Include full file text when small; otherwise include head/tail with a marker.
truncate_for_prompt() {
	local file="$1"
	local max_chars="$2"
	local size

	size=$(wc -c <"$file")

	if ((size <= max_chars)); then
		cat "$file"
		return
	fi

	local half=$((max_chars / 2))

	head -c "$half" "$file"
	printf "\n\n...[truncated output: first and last %d bytes of %d total]...\n\n" "$half" "$size"
	tail -c "$half" "$file"
}

# ---------------------------------------------------------------------------
# Parse flags
# ---------------------------------------------------------------------------
while getopts "n:t:o:s:f:m:kh" opt; do
	case "$opt" in
	n) PARALLEL="$OPTARG" ;;
	t) TIMEOUT="$OPTARG" ;;
	o) OUTDIR="$OPTARG" ;;
	s) SKIP_LIST="$OPTARG" ;;
	f) FILTER_LIST="$OPTARG" ;;
	m) VERIFY_MODEL="$OPTARG" ;;
	k) KEEP_GOING=true ;;
	h) usage ;;
	*) usage ;;
	esac
done

# ---------------------------------------------------------------------------
# Resolve paths
# ---------------------------------------------------------------------------
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
EXAMPLES_DIR="$REPO_ROOT/examples"
[[ -d "$EXAMPLES_DIR" ]] || die "examples/ directory not found at $EXAMPLES_DIR"

if [[ -z "$OUTDIR" ]]; then
	OUTDIR="/tmp/sdk-example-tests-$(date +%Y%m%d-%H%M%S)"
fi

mkdir -p "$OUTDIR"

# Verify prerequisites.
command -v go >/dev/null 2>&1 || die "go not found in PATH"
command -v codex >/dev/null 2>&1 || die "codex CLI not found in PATH"
command -v jq >/dev/null 2>&1 || die "jq not found in PATH"
command -v timeout >/dev/null 2>&1 || die "timeout not found in PATH"

# Verifier schema for Codex structured output.
VERIFIER_SCHEMA="$OUTDIR/verifier-schema.json"
cat >"$VERIFIER_SCHEMA" <<'JSON'
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "pass": { "type": "boolean" },
    "reason": { "type": "string" }
  },
  "required": ["pass", "reason"]
}
JSON

# ---------------------------------------------------------------------------
# Discover examples
# ---------------------------------------------------------------------------
examples=()

for dir in "$EXAMPLES_DIR"/*/; do
	name="$(basename "$dir")"

	# Skip non-runnable dirs.
	if in_list "$name" "$SKIP_DIRS"; then
		continue
	fi

	# Must contain main.go.
	[[ -f "$dir/main.go" ]] || continue

	# User-specified skip list.
	if in_list "$name" "$SKIP_LIST"; then
		continue
	fi

	# User-specified filter list.
	if [[ -n "$FILTER_LIST" ]] && ! in_list "$name" "$FILTER_LIST"; then
		continue
	fi

	examples+=("$name")
done

# Sort for deterministic order.
mapfile -t examples < <(printf '%s\n' "${examples[@]}" | sort)

total=${#examples[@]}
[[ $total -gt 0 ]] || die "No examples found to run"

echo "=== SDK Example Tests ==="
echo "Examples to run: $total"
echo "Parallel: $PARALLEL"
echo "Timeout per example: ${TIMEOUT}s"
echo "Output dir: $OUTDIR"
if [[ -n "$VERIFY_MODEL" ]]; then
	echo "Verify model: $VERIFY_MODEL"
fi
echo ""

# ---------------------------------------------------------------------------
# Per-example arguments and run commands
# ---------------------------------------------------------------------------
example_args() {
	local name="$1"

	case "$name" in
	cancellation | client_multi_turn | extended_thinking | setting_sources | tools_option)
		echo "all"
		;;
	*)
		echo ""
		;;
	esac
}

run_example() {
	local name="$1"
	local logfile="$2"
	local args

	args="$(example_args "$name")"

	# Examples with their own go.mod are separate modules and must be
	# built from within their directory.
	local run_dir="$REPO_ROOT"
	local run_pkg="./examples/$name"

	if [[ -f "$EXAMPLES_DIR/$name/go.mod" ]]; then
		run_dir="$EXAMPLES_DIR/$name"
		run_pkg="."
	fi

	if [[ -n "$args" ]]; then
		(cd "$run_dir" && timeout "$TIMEOUT" go run "$run_pkg" "$args") >"$logfile" 2>&1
	else
		(cd "$run_dir" && timeout "$TIMEOUT" go run "$run_pkg") >"$logfile" 2>&1
	fi
}

# ---------------------------------------------------------------------------
# Parse a codex --json result file into verdict + reason.
# Writes two files: <name>.verdict ("true"/"false") and <name>.reason
# ---------------------------------------------------------------------------
parse_result() {
	local name="$1"
	local result_file="$OUTDIR/${name}.result.jsonl"

	if [[ ! -s "$result_file" ]]; then
		echo "false" >"$OUTDIR/${name}.verdict"
		echo "Codex verification failed (empty result)" >"$OUTDIR/${name}.reason"
		return
	fi

	local raw inner verdict reason

	raw="$(jq -r 'select(.type=="item.completed" and .item.type=="agent_message") | .item.text // empty' "$result_file" \
		2>/dev/null | tail -n 1)"

	if [[ -z "$raw" ]]; then
		echo "false" >"$OUTDIR/${name}.verdict"
		echo "Codex verification failed (no agent_message found)" >"$OUTDIR/${name}.reason"
		return
	fi

	# Handle fallback formats if JSON is wrapped in markdown or extra text.
	inner="$(echo "$raw" | sed -n '/^```/,/^```/{/^```/d;p;}')"
	if [[ -z "$inner" ]]; then
		inner="$raw"
	fi

	if ! echo "$inner" | jq -e . >/dev/null 2>&1; then
		inner="$(echo "$inner" | tr -d '\n' | sed -n 's/.*\({.*}\).*/\1/p' | head -n 1)"
	fi

	if ! echo "$inner" | jq -e . >/dev/null 2>&1; then
		echo "false" >"$OUTDIR/${name}.verdict"
		echo "Codex verification failed (invalid JSON payload)" >"$OUTDIR/${name}.reason"
		return
	fi

	verdict="$(echo "$inner" | jq -r '.pass // false' 2>/dev/null || echo "false")"
	reason="$(echo "$inner" | jq -r '.reason // "Unknown"' 2>/dev/null || echo "Unknown")"

	echo "$verdict" >"$OUTDIR/${name}.verdict"
	echo "$reason" >"$OUTDIR/${name}.reason"
}

# ---------------------------------------------------------------------------
# Worker: run one example end-to-end (run + verify), write results to files.
# Runs in a subshell via &.
# ---------------------------------------------------------------------------
process_example() {
	local name="$1"
	local logfile="$OUTDIR/${name}.log"
	local result_file="$OUTDIR/${name}.result.jsonl"
	local verify_err_file="$OUTDIR/${name}.verify.err"

	# --- Run ---------------------------------------------------------------
	local run_rc=0
	run_example "$name" "$logfile" || run_rc=$?

	if [[ $run_rc -eq 124 ]]; then
		echo "false" >"$OUTDIR/${name}.verdict"
		echo "Runtime timeout after ${TIMEOUT}s" >"$OUTDIR/${name}.reason"
		return
	fi

	if [[ $run_rc -ne 0 ]]; then
		if grep -q "^panic:" "$logfile" 2>/dev/null; then
			echo "false" >"$OUTDIR/${name}.verdict"
			echo "Runtime error (exit code $run_rc) - panic detected" >"$OUTDIR/${name}.reason"
			return
		fi

		echo "false" >"$OUTDIR/${name}.verdict"
		echo "Runtime error (exit code $run_rc)" >"$OUTDIR/${name}.reason"
		return
	fi

	# --- Verify with Codex -------------------------------------------------
	local source_code output_log prompt
	source_code="$(cat "$EXAMPLES_DIR/$name/main.go")"
	output_log="$(truncate_for_prompt "$logfile" "$VERIFY_MAX_CHARS")"

	prompt="$(cat <<PROMPT
Below is the Go source code for an SDK example named "$name" and its output log.

Determine if the example ran successfully and produced output consistent with
what the source code intends to demonstrate.

Important context:
- The example calls a live LLM, so exact wording can vary.
- Focus ONLY on the OUTPUT LOG (runtime behavior), not whether source style is ideal.
- For examples that intentionally show errors/cancellation/denials, expected error output is NOT failure.

Evaluate:
1) Did it complete without crashing or timing out?
2) Does the output structure align with what the code prints (sections/labels/fields)?
3) Are major expected signals present (assistant output, result completion, or clearly intended demo output)?

Respond with JSON only:
{"pass": true/false, "reason": "short explanation"}

SOURCE CODE:
$source_code

OUTPUT LOG:
$output_log
PROMPT
)"

	local -a verify_cmd
	verify_cmd=(
		codex exec
		--json
		--full-auto
		--ephemeral
		--skip-git-repo-check
		--sandbox read-only
		--cd "$REPO_ROOT"
		--output-schema "$VERIFIER_SCHEMA"
	)

	if [[ -n "$VERIFY_MODEL" ]]; then
		verify_cmd+=(-m "$VERIFY_MODEL")
	fi

	verify_cmd+=("$prompt")

	"${verify_cmd[@]}" >"$result_file" 2>"$verify_err_file" || true

	# --- Parse result ------------------------------------------------------
	parse_result "$name"
}

# ---------------------------------------------------------------------------
# Main loop - run workers with bounded parallelism
# ---------------------------------------------------------------------------

# Remove finished PIDs from the WORKER_PIDS array.
reap_workers() {
	local alive=()

	for pid in "${WORKER_PIDS[@]}"; do
		if kill -0 "$pid" 2>/dev/null; then
			alive+=("$pid")
		fi
	done

	WORKER_PIDS=("${alive[@]+"${alive[@]}"}")
}

# Wait until worker count drops below PARALLEL.
wait_for_slot() {
	while true; do
		reap_workers
		[[ ${#WORKER_PIDS[@]} -lt $PARALLEL ]] && return
		sleep 0.2
	done
}

for name in "${examples[@]}"; do
	[[ "$INTERRUPTED" == true ]] && break

	wait_for_slot
	[[ "$INTERRUPTED" == true ]] && break

	echo "  Starting $name..."
	process_example "$name" &
	WORKER_PIDS+=($!)
done

# Wait for all remaining workers.
for pid in "${WORKER_PIDS[@]}"; do
	wait "$pid" 2>/dev/null || true
done

# ---------------------------------------------------------------------------
# Collect results and print summary
# ---------------------------------------------------------------------------
pass_count=0
fail_count=0

echo ""
echo "=== Example Test Results ==="

for name in "${examples[@]}"; do
	local_verdict="$(cat "$OUTDIR/${name}.verdict" 2>/dev/null || echo "false")"
	local_reason="$(cat "$OUTDIR/${name}.reason" 2>/dev/null || echo "No result")"

	if [[ "$local_verdict" == "true" ]]; then
		printf "  %-30s PASS  %s\n" "$name" "$local_reason"
		((pass_count++)) || true
	else
		printf "  %-30s FAIL  %s\n" "$name" "$local_reason"
		((fail_count++)) || true

		if [[ "$KEEP_GOING" != true ]]; then
			break
		fi
	fi
done

echo "=== $pass_count/$total passed, $fail_count failed ==="
echo ""
echo "Logs: $OUTDIR"

# Exit non-zero if any failures.
[[ $fail_count -eq 0 ]]
