#!/usr/bin/env bash
# The major hold's verdict: does this release run cut a tag, or does it stand
# down while main carries a breaking change? Driven by
# .github/workflows/version.yml, which gathers the facts from the tree
# release-it is about to release and hands them over as environment variables.
#
# Why it is a file: the verdict decides whether a version tag is cut at all,
# and as inline workflow bash nothing in the tree could redden on it — not the
# exact-version rule, not the numeric major compare, not the refusal of a
# non-major dispatch. internal/ciguard/release_major_hold_test.go runs THIS
# script over crafted facts.
#
# The rule: release-it cuts a major from a single `feat!:` commit, and an
# explicit dispatch increment overrides the bump its changelog recommends
# (@release-it/conventional-changelog reads the explicit increment last) — so
# the first `!` after v7 would auto-cut v8, and a dispatched `minor` would ship
# the breaking changes under a 7.x minor. This release hold answers every run
# instead:
#   - an automatic run (the release that follows a green heavy run of main)
#     skips GREEN with a notice naming the held version and the dispatch
#     that cuts it;
#   - a dispatched patch, minor, explicit same-major version or blank input
#     from main is refused while a major is pending;
#   - only the exact N+1.0.0 dispatch cuts a major (`major` and every version
#     above N are refused with that message);
#   - a ref other than main is not guarded here — the hold protects main
#     alone, and the release/* line owns its own rules.
# The release hold (this script, automatic, from the first `!`) and the cut
# gate (the operator's go/no-go checklist) are two different events; this
# script is only the former.
#
# Environment — every value as the workflow gathers it:
#   EVENT        github.event_name: workflow_run (the automatic release) or
#                workflow_dispatch.
#   REF          the branch being released (`git rev-parse --abbrev-ref
#                HEAD`).
#   INPUT        the workflow_dispatch `version` input; empty on every other
#                event.
#   CURRENT      the latest released version, bare X.Y.Z (`git describe
#                --tags --abbrev=0` with the leading `v` stripped).
#   RECOMMENDED  the version release-it computes with no increment
#                (`release-it --release-version --ci`); empty when no commit
#                since the last tag bumps.
#
# Output on stdout, exactly one line, for the caller to append to
# $GITHUB_OUTPUT:
#   verdict=release|skip
# Everything else goes to stderr: it belongs in the job log, not in the
# caller's parsing.
#
# Exit code: 0 with a verdict, 1 when the request is refused (the step fails
# and release-it is skipped), 2 when the facts cannot support any verdict —
# a hold that answers on a guess releases on a guess.
set -euo pipefail

say() { printf '%s\n' "$*" >&2; }

refuse() {
  echo "::error::$*" >&2
  exit 1
}

# unusable <message> — the facts cannot support any verdict, release or skip.
unusable() {
  say "release-major-hold: $*"
  exit 2
}

VERSION_RE='^[0-9]+\.[0-9]+\.[0-9]+$'

event="${EVENT:-}"
ref="${REF:-}"
input="${INPUT:-}"
current="${CURRENT:-}"
recommended="${RECOMMENDED:-}"

case "$event" in
  workflow_run|workflow_dispatch) ;;
  *) unusable "EVENT is '${event}', expected workflow_run or workflow_dispatch" ;;
esac
[ -n "$ref" ] || unusable "REF is empty — the hold judges the branch being released, none was given"
printf '%s' "$current" | grep -Eq "$VERSION_RE" || unusable "CURRENT is '${current}', expected a bare X.Y.Z version"
[ -z "$recommended" ] || printf '%s' "$recommended" | grep -Eq "$VERSION_RE" || unusable "RECOMMENDED is '${recommended}', expected a bare X.Y.Z version or empty"
case "$event" in
  workflow_run)
    [ -z "$input" ] || unusable "EVENT is $event, which carries no version input, but INPUT is '${input}'"
    ;;
esac

major="${current%%.*}"
next=$((major + 1))

# A major is pending when the recommended bump leaves the current major. The
# compare is numeric on purpose: a string compare reads 10.0.0 as below 9.x.
held=false
if [ -n "$recommended" ] && [ "${recommended%%.*}" -gt "$major" ]; then
  held=true
fi

# The hold guards main only; every other ref releases as it always did.
if [ "$ref" != "main" ]; then
  say "release-major-hold: ref is ${ref}, not main — the release hold guards main alone"
  printf 'verdict=release\n'
  exit 0
fi

case "$event" in
  workflow_run)
    if [ "$held" = true ]; then
      say "::notice::Release held: the commits since v${current} recommend v${recommended}, a major. No tag is cut; an operator cuts it by dispatching this workflow with version ${next}.0.0."
      printf 'verdict=skip\n'
      exit 0
    fi
    printf 'verdict=release\n'
    exit 0
    ;;
esac

# workflow_dispatch: an operator's explicit request. Its one door to a major
# is the exact next version.
if [ "$input" = "${next}.0.0" ]; then
  printf 'verdict=release\n'
  exit 0
fi

if [ "$input" = "major" ]; then
  refuse "a major is cut by dispatching exactly ${next}.0.0, not with the word 'major'"
fi

leading="${input%%.*}"
case "$leading" in
  ''|*[!0-9]*) ;;
  *)
    if [ "$leading" -gt "$major" ]; then
      refuse "a major is cut by dispatching exactly ${next}.0.0 — received '${input}' claims a version above v${major}"
    fi
    ;;
esac

# Every remaining input — patch, minor, an explicit version of the current
# major, an empty input (the recommended bump) — would ship the pending
# breaking changes under the old major.
if [ "$held" = true ]; then
  refuse "main is held for v${next}: a patch or minor cut from main would ship its breaking changes under v${major}. Dispatch version ${next}.0.0 to cut the major, or release the fix from release/v${major} — received '${input}'"
fi
printf 'verdict=release\n'
