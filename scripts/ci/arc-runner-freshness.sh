#!/usr/bin/env bash
# Is the runner image the ARC pools run the image this tree defines?
#
# The pools' image is pinned by version and digest OUTSIDE this repository (the
# operator's infrastructure, out of Renovate's sight), and a stale pin fails
# quietly: jobs download the actions the image no longer bakes (codeload 429s,
# #2435), set up a Go or Node the image does not carry, or — the 2025 deadlock —
# run a runner version GitHub has stopped accepting. This script compares, with
# no network, what the running image records about itself with what HEAD's
# ci/arc-runner/ would build, and names every fact that drifted.
#
# Usage: arc-runner-freshness.sh <repo-root>
#
# Image facts (paths overridable for the ciguard harness):
#   IMAGE_ENV_FILE      GO_VERSION= / NODE_VERSION= baked by the Dockerfile
#                       (default $RUNNER_TOOL_CACHE/iterion-ci-runner.env)
#   IMAGE_ACTIONS_FILE  the baked-actions manifest the image was built from
#                       (default /opt/hostedtoolcache/iterion-ci-runner-actions.txt)
#   IMAGE_RUNNER_VERSION what the runner answers to --version, log lines
#                       included (default: Runner.Listener --version)
#
# Output: one `ok <fact>` or `::error::stale <fact>: …` line per fact on stdout.
# Exit 0 when the image matches HEAD, 1 when any fact drifted, 2 when a fact
# cannot be read (an unreadable fact is not a fresh one).
set -euo pipefail

root=${1:?usage: arc-runner-freshness.sh <repo-root>}
dockerfile="$root/ci/arc-runner/Dockerfile"
manifest="$root/ci/arc-runner/baked-actions.txt"
env_file=${IMAGE_ENV_FILE:-${RUNNER_TOOL_CACHE:-/opt/hostedtoolcache}/iterion-ci-runner.env}
actions_file=${IMAGE_ACTIONS_FILE:-/opt/hostedtoolcache/iterion-ci-runner-actions.txt}

unreadable() {
	printf '::error::arc-runner-freshness: %s\n' "$*"
	exit 2
}

# The value of `ARG <name>=<value>` in HEAD's Dockerfile.
head_arg() {
	local v
	v=$(sed -n "s/^ARG $1=\\(.*\\)$/\\1/p" "$dockerfile" | head -n 1)
	[ -n "$v" ] || unreadable "HEAD's $dockerfile declares no ARG $1"
	printf '%s' "$v"
}

# The value of `<name>=<value>` in the image's env file.
image_env() {
	local v
	v=$(sed -n "s/^$1=\\(.*\\)$/\\1/p" "$env_file" | head -n 1)
	[ -n "$v" ] || unreadable "the image's $env_file records no $1"
	printf '%s' "$v"
}

[ -r "$dockerfile" ] || unreadable "cannot read $dockerfile"
[ -r "$manifest" ] || unreadable "cannot read $manifest"
[ -r "$env_file" ] || unreadable "the image carries no $env_file (built before the tool cache was baked?)"
[ -r "$actions_file" ] || unreadable "the image carries no $actions_file (built before the actions were baked?)"

head_runner=$(sed -n 's|^FROM ghcr\.io/actions/actions-runner:\([0-9][0-9.]*\)@.*|\1|p' "$dockerfile" | head -n 1)
[ -n "$head_runner" ] || unreadable "HEAD's $dockerfile has no pinned FROM ghcr.io/actions/actions-runner:<version>@<digest>"
# Inside a job, Runner.Listener logs to stdout before it answers
# ("[RUNNER … INFO HostContext] No proxy settings…"): keep the version line.
if [ -n "${IMAGE_RUNNER_VERSION:-}" ]; then
	raw_runner=$IMAGE_RUNNER_VERSION
else
	raw_runner=$(/home/runner/bin/Runner.Listener --version 2>/dev/null) || unreadable "Runner.Listener --version failed"
fi
image_runner=$(printf '%s\n' "$raw_runner" | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | tail -n 1 || true)
[ -n "$image_runner" ] || unreadable "no version line in the runner's answer: $(printf '%s' "$raw_runner" | head -c 200)"

stale=0
check() { # fact image head
	if [ "$2" = "$3" ]; then
		printf 'ok %s %s\n' "$1" "$2"
	else
		printf '::error::stale %s: the pools run %s, HEAD builds %s — bump the pool image pin\n' "$1" "$2" "$3"
		stale=1
	fi
}

check runner-version "$image_runner" "$head_runner"
check go-version "$(image_env GO_VERSION)" "$(head_arg GO_VERSION)"
check node-version "$(image_env NODE_VERSION)" "$(head_arg NODE_VERSION)"
if cmp -s "$actions_file" "$manifest"; then
	printf 'ok baked-actions %s entries\n' "$(grep -c . "$manifest")"
else
	printf '::error::stale baked-actions: the image bakes a different action set than HEAD'"'"'s workflows use — jobs download them from codeload again; bump the pool image pin\n'
	diff "$actions_file" "$manifest" | sed 's/^/  /' || true
	stale=1
fi
exit "$stale"
