#!/usr/bin/env bash
# The manifest of third-party actions the ARC runner image bakes: every
# `uses:` of this repository's workflows, as the runner's action archive cache
# keys them.
#
# Why a generated file (#2435): ephemeral runners download every action from
# codeload.github.com on every job, and codeload answers 429 under load — four
# merge-queue ejections on 2026-10-15, and a third party measured the same
# throttling with no ARC involved. Baking the archives into the image is the
# only remedy that does not depend on a service we do not control. The image
# must bake exactly what the workflows ask for, so the list is derived, never
# hand-kept: ci/arc-runner/baked-actions.txt is this script's output, and
# internal/ciguard runs THIS script to judge it.
#
# Usage: baked-actions-manifest.sh [workflow-dir]   (default: this repo's)
#
# Output, one line per `uses:` after sorting and deduplication:
#
#   <owner>/<repo> <40-char sha> <subpath>
#
# The first two fields are the cache key — the runner looks for
# <cache>/<owner>_<repo>/<sha>.tar.gz, one archive per repository — and
# <subpath> is where the action's own manifest sits inside it (`.` for an
# action at the repository root), which is what lets a verifier prove every
# `uses:` resolves from the baked archive with no network.
#
# Exit code: 0 with a manifest, or 1 naming the file and line of a `uses:`
# this cache cannot key. A tag is not a cache key: the runner asks the Actions
# service to resolve a ref to a sha and looks the SHA up, so a tag-pinned
# action would silently miss the cache and go back to codeload. Everything in
# this tree is already pinned (internal/ciguard holds the publish pipeline to
# it); the refusal keeps it that way instead of quietly shipping a hole.
set -euo pipefail

dir=${1:-"$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)/.github/workflows"}
[ -d "$dir" ] || { echo "no such workflow directory: $dir" >&2; exit 1; }

shopt -s nullglob
files=("$dir"/*.yml "$dir"/*.yaml)
[ "${#files[@]}" -gt 0 ] || { echo "no workflow file in $dir" >&2; exit 1; }

# The manifest is buffered, so a refusal below writes nothing to stdout: a
# caller redirecting this script into the committed file never gets half a
# manifest.
manifest=$(awk '
  # A `uses:` step or a reusable-workflow call, with or without the list dash.
  /^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[^[:space:]]/ {
    line = $0
    sub(/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*/, "", line)
    sub(/[[:space:]].*$/, "", line)          # drop the trailing `# v7.0.1`
    sub(/\r$/, "", line)
    if (line ~ /^["'"'"']/) { gsub(/^["'"'"']|["'"'"']$/, "", line) }
    if (line ~ /^\.\//) next                 # a workflow of this repository
    if (line == "") next
    if (line !~ /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+(\/[A-Za-z0-9_.\/-]+)?@[0-9a-f]{40}$/) {
      printf "%s:%d: `uses: %s` cannot be baked: pin the action by its 40-character commit sha — the action archive cache is keyed by the sha the Actions service resolves, so a tag misses it and goes back to codeload\n", FILENAME, FNR, line > "/dev/stderr"
      bad = 1
      next
    }
    at = index(line, "@")
    sha = substr(line, at + 1)
    ref = substr(line, 1, at - 1)
    n = split(ref, part, "/")
    path = "."
    if (n > 2) {
      path = part[3]
      for (i = 4; i <= n; i++) path = path "/" part[i]
    }
    # A reusable WORKFLOW of another repository is not an action: the service
    # fetches it when it builds the plan, the worker never puts it in
    # _actions, and no archive cache is consulted for it.
    if (path ~ /\.(yml|yaml)$/) next
    repo = part[1] "/" part[2]
    # One repository, one spelling. The cache key is the name the Actions
    # service resolves, so the canonical spelling is the only one that can
    # match under either reading of it; two spellings in the tree would bake
    # one archive twice and leave one of them unreachable — a silent fallback
    # to codeload that looks exactly like a working cache.
    key = tolower(repo)
    if (key in spelling) {
      if (spelling[key] != repo) {
        printf "%s:%d: `uses: %s` spells %s, and %s is spelled %s elsewhere in these workflows: use the spelling GitHub itself reports in both, or one of the two baked archives will never be looked up\n", FILENAME, FNR, line, repo, key, spelling[key] > "/dev/stderr"
        bad = 1
        next
      }
    } else {
      spelling[key] = repo
    }
    print repo, sha, path
  }
  END { if (bad) exit 1 }
' "${files[@]}")

[ -n "$manifest" ] || { echo "no sha-pinned action found in $dir — this script would bake nothing" >&2; exit 1; }
printf '%s\n' "$manifest" | LC_ALL=C sort -u
