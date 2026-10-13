#!/usr/bin/env bash
# Resolve which release tag the Homebrew tap should carry — brew-update.yml's
# first decision.
#
# Usage: scripts/brew-tap-resolve.sh   (from the repository root)
#
# Environment:
#   INPUT_TAG      optional: the tag an operator dispatched (a backfill).
#   GITHUB_OUTPUT  where `tag=<tag>` and/or `skip=1` are written.
#   GH_TOKEN       read by `gh release view`.
#
# Without INPUT_TAG it picks the NEWEST tag, at or above the version the
# formula carries, whose release holds the four CLI binaries the formula
# needs. A tag still building — the nightly's own, when this runs right after
# it — or whose build failed is passed over, so it never stalls the tap.
# Version order (sort -V), not creation order: a maintenance tag cut after a
# newer release never walks the tap backwards. A pre-release is never tapped
# (brew-tap-pr.sh refuses one, and sort -V would rank it above its own final).
# A tag whose release cannot be read fails the run rather than being skipped
# in silence.
#
# `skip=1` when there is nothing to do: no tag with complete artefacts, a tag
# not newer than the tap, a tap whose formula AND cask already carry it, a
# cask at or ahead of the tag (a desktop-only release dispatched into it is
# never walked back), or a cask left behind whose release still holds no
# desktop ZIP to catch up with.
set -euo pipefail

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

cli_assets=(iterion-darwin-arm64 iterion-darwin-amd64 iterion-linux-arm64 iterion-linux-amd64)
desktop_zip=iterion-desktop-darwin-universal.zip

version_of() {
  sed -n 's/^[[:space:]]*version[[:space:]]*"\(.*\)".*/\1/p' "$1" | head -n1 || true
}

skip() {
  echo "$*"
  echo "skip=1" >>"$GITHUB_OUTPUT"
  exit 0
}

current_formula="$(version_of Formula/iterion.rb)"
current_cask="$(version_of Cask/iterion-desktop.rb)"

if [ -n "${INPUT_TAG:-}" ]; then
  tag="$INPUT_TAG"
  case "${tag#v}" in
  *-*) skip "not tapping ${tag}: a pre-release never takes the tap" ;;
  esac
else
  tags="$(git tag --list 'v*' | sort -V -r)"
  tag=""
  while read -r t; do
    [ -n "$t" ] || continue
    v="${t#v}"
    case "$v" in
    *-*) continue ;;
    esac
    if [ "$v" != "${current_formula:-0}" ] &&
      [ "$(printf '%s\n%s\n' "${current_formula:-0}" "$v" | sort -V | tail -n1)" != "$v" ]; then
      break
    fi
    assets="$(gh release view "$t" --json assets --jq '.assets[].name' </dev/null)" ||
      { echo "::error::cannot read the release of $t" >&2; exit 1; }
    missing=""
    for a in "${cli_assets[@]}"; do
      grep -qxF "$a" <<<"$assets" || missing="$missing $a"
    done
    if [ -z "$missing" ]; then
      tag="$t"
      break
    fi
    echo "$t: CLI artefacts incomplete (missing:$missing) — still building, or a failed build; trying the tag before it"
  done <<<"$tags"
fi

[ -n "$tag" ] ||
  skip "no v* tag at or above the tap's ${current_formula:-version} has complete CLI artefacts; nothing to tap"

echo "resolved tag=$tag"
echo "tag=$tag" >>"$GITHUB_OUTPUT"
vtag="${tag#v}"

# Downgrade guard, for a dispatched tag above all: sort -V compares semver,
# not strings.
newest="$(printf '%s\n%s\n' "${current_formula:-0}" "$vtag" | sort -V | tail -n1)"
[ "$newest" = "$vtag" ] ||
  skip "tag $tag is not newer than the tap's $current_formula — nothing to update"

# The formula already carries the tag: only the cask can move. Skipping on
# the formula alone froze the desktop cask for a whole release cycle once
# (measured), so a cask left behind is caught up — but only once the release
# holds the desktop ZIP: before that the run would rewrite nothing, and
# brew-tap-pr.sh would be handed the tag's branch with nothing new on it.
if [ "$vtag" = "$current_formula" ]; then
  if [ -z "$current_cask" ] || [ "$vtag" = "$current_cask" ]; then
    skip "the tap already carries $vtag (formula and cask); nothing to update"
  fi
  if [ "$(printf '%s\n%s\n' "$current_cask" "$vtag" | sort -V | tail -n1)" != "$vtag" ]; then
    skip "the cask ($current_cask) is ahead of $tag; nothing to catch up"
  fi
  if [ -z "${assets+x}" ]; then
    assets="$(gh release view "$tag" --json assets --jq '.assets[].name' </dev/null)" ||
      { echo "::error::cannot read the release of $tag" >&2; exit 1; }
  fi
  grep -qxF "$desktop_zip" <<<"$assets" ||
    skip "the formula carries $vtag; the cask ($current_cask) waits for $tag's desktop ZIP, which its release does not hold yet"
fi
