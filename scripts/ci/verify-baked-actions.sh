#!/usr/bin/env bash
# Prove, with no network, that every `uses:` of this repository resolves from
# the action archives baked into the ARC runner image.
#
# Usage: verify-baked-actions.sh <cache-dir> <manifest-file>
#
# <cache-dir> is what the image sets ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE to,
# and the layout is the runner's, not ours (actions/runner 2.337.0,
# src/Runner.Worker/ActionManager.cs): for `uses: <owner>/<repo>/<path>@<sha>`
# the worker looks for <cache>/<owner>_<repo>/<sha>.tar.gz and copies it into
# the job's temp directory, extracting it exactly as it would a codeload
# download. One archive per repository, keyed by the sha the Actions service
# resolved — which is the pin itself, since every `uses:` here is sha-pinned.
#
# What it checks, per manifest line:
#   - the archive exists and is not empty;
#   - it unpacks to exactly ONE top-level directory (the runner requires that
#     nested folder: a repository archive from GitHub always has one, and the
#     symlink path refuses anything else);
#   - the action's own manifest is inside it at the `uses:` subpath, so the
#     step would find action.yml without reaching codeload.
#
# It reads the archives and never writes: the cache is read-only for the
# runner too (the worker copies out of it).
#
# Exit code: 0 when every entry resolves, 1 naming every one that does not. An
# empty manifest is a refusal, not a pass — a verifier that verified nothing
# must not report success.
set -euo pipefail

cache=${1:?usage: verify-baked-actions.sh CACHE-DIR MANIFEST-FILE}
manifest=${2:?usage: verify-baked-actions.sh CACHE-DIR MANIFEST-FILE}

[ -d "$cache" ] || { echo "no action archive cache at $cache" >&2; exit 1; }
[ -f "$manifest" ] || { echo "no manifest at $manifest" >&2; exit 1; }

failed=0
checked=0
while read -r repo sha path; do
	case "$repo" in '' | '#'*) continue ;; esac
	if [ -z "${sha:-}" ] || [ -z "${path:-}" ]; then
		echo "$manifest: malformed line for $repo — expected '<owner>/<repo> <sha> <subpath>'" >&2
		failed=1
		continue
	fi
	checked=$((checked + 1))
	archive="$cache/${repo//\//_}/$sha.tar.gz"
	if [ ! -s "$archive" ]; then
		echo "$repo@$sha: no baked archive at $archive — every job using it would go back to codeload.github.com, which is what #2435 is about" >&2
		failed=1
		continue
	fi
	listing=$(tar -tzf "$archive") || { echo "$repo@$sha: $archive is not a readable tar.gz" >&2; failed=1; continue; }
	roots=$(printf '%s\n' "$listing" | sed -n 's|^\([^/]*\)/.*|\1|p' | sort -u)
	if [ "$(printf '%s\n' "$roots" | grep -c .)" -ne 1 ]; then
		echo "$repo@$sha: $archive unpacks to $(printf '%s\n' "$roots" | grep -c .) top-level directories, the runner requires exactly one nested folder" >&2
		failed=1
		continue
	fi
	if [ "$path" = "." ]; then
		want="$roots/action.y"
	else
		want="$roots/$path/action.y"
	fi
	if ! printf '%s\n' "$listing" | grep -qx -e "${want}ml" -e "${want}aml"; then
		echo "$repo@$sha: $archive carries no ${want#"$roots"/}ml — the action this manifest line was generated from would not resolve from the cache" >&2
		failed=1
	fi
done < "$manifest"

if [ "$checked" -eq 0 ]; then
	echo "$manifest lists no action to verify" >&2
	exit 1
fi
[ "$failed" -eq 0 ] || exit 1
echo "every one of the $checked baked action(s) resolves from $cache with no network" >&2
