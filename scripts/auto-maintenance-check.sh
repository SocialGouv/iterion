#!/usr/bin/env bash
# auto-maintenance-check — the agreements an unattended dependency loop rests on,
# run as a preflight that REDDENS.
#
# The loop's premise is that a dependency PR can open, be audited, be aligned,
# prove itself through the repository's own required checks and merge with nobody
# watching. That premise is not one setting: it is a handful of settings in three
# different systems (the repository's ruleset, the iterion integration, the
# GitHub Apps' installations) that must agree. Nothing type-checks them, and the
# headline friction of this work was born exactly there — a rename applied to one
# of two sites, with no repository able to see the disagreement (see the
# `revi/review` vs `iterion/review` replay in the tests).
#
# Prose in a runbook would not have caught it. This does.
#
# Usage:
#   scripts/auto-maintenance-check.sh <owner/repo> [--json]
#
# Environment (all optional; each default is the normal case):
#   AUTO_MAINT_GH         the gh binary                        (default: gh)
#   AUTO_MAINT_ITERION    argv that prints the repo-bots JSON   (default: iterion remote forge repo-bots --json)
#   AUTO_MAINT_BOTS_DIR   the catalog to read bot defaults from (default: <repo root>/bots)
#   AUTO_MAINT_PR_SAMPLE  how many recent PRs to sample         (default: 20)
#   AUTO_MAINT_PAT        a classic PAT for the one read an OAuth token cannot do
#                         (agreement 6; the value is used by reference, never printed)
#
# Exit codes — a disagreement and a blind spot are NOT the same answer:
#   0  every agreement holds
#   1  at least one agreement is BROKEN (measured disagreement)
#   2  fail-closed: a tool is missing, an API did not answer, a page was
#      truncated, or a payload was not valid JSON — the agreement is UNKNOWN,
#      and unknown never becomes "no disagreement"
#   3  usage error
#
# It reads. It never writes: no ruleset patch, no integration patch, no merge.
set -euo pipefail

# ---------------------------------------------------------------------------
# Plumbing
# ---------------------------------------------------------------------------

GH="${AUTO_MAINT_GH:-gh}"
PR_SAMPLE="${AUTO_MAINT_PR_SAMPLE:-20}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOTS_DIR="${AUTO_MAINT_BOTS_DIR:-$ROOT/bots}"

JSON_OUT=0
REPO=""

# usage prints the header comment block, whatever its length. A hardcoded line
# range drifts the first time someone edits the header — and it had, silently
# dropping the line that documents exit code 3 from the very message that
# returns it.
usage() { awk 'NR > 1 { if ($0 !~ /^#/) exit; sub(/^#[[:space:]]?/, ""); print }' "${BASH_SOURCE[0]}"; }

for arg in "$@"; do
  case "$arg" in
    --json) JSON_OUT=1 ;;
    -h|--help) usage; exit 0 ;;
    -*) printf 'unknown flag: %s\n' "$arg" >&2; exit 3 ;;
    *) [ -n "$REPO" ] && { printf 'one repository at a time, got %s and %s\n' "$REPO" "$arg" >&2; exit 3; }
       REPO="$arg" ;;
  esac
done
[ -n "$REPO" ] || { usage >&2; exit 3; }
case "$REPO" in */*) ;; *) printf 'expected owner/repo, got %s\n' "$REPO" >&2; exit 3 ;; esac
ORG="${REPO%%/*}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# fatal exits 2 — the fail-closed answer. Anything this script cannot READ is
# reported as unknown and blocks; it is never folded into a green.
fatal() { printf 'BLIND SPOT: %s\n' "$*" >&2; exit 2; }

need_tool() {
  command -v "$1" >/dev/null 2>&1 || fatal "missing tool: $1"
}

# api <dest> <path...> — one JSON read, paginated, validated.
#
# --paginate is not an optimisation here: a read that silently stops at page 1
# answers "no disagreement" for everything on page 2. And the payload is parsed
# before it is used, because `gh` prints an error object with exit 0 on some
# paths.
api() {
  local dest="$1"; shift
  if ! "$GH" api --paginate "$@" >"$dest" 2>"$dest.err"; then
    fatal "$GH api $* did not answer: $(tr '\n' ' ' <"$dest.err")"
  fi
  [ -s "$dest" ] || fatal "$GH api $* answered with an empty body"
  jq -e . <"$dest" >/dev/null 2>&1 || fatal "$GH api $* did not answer valid JSON"
}

# api_bounded <dest> <path> — a read whose BOUND is the point.
#
# Deliberately not paginated: the PR sample is "the last N", and --paginate
# would walk every pull request the repository ever had. The two helpers are
# separate so the choice is visible at each call site rather than implied.
api_bounded() {
  local dest="$1"; shift
  if ! "$GH" api "$@" >"$dest" 2>"$dest.err"; then
    fatal "$GH api $* did not answer: $(tr '\n' ' ' <"$dest.err")"
  fi
  [ -s "$dest" ] || fatal "$GH api $* answered with an empty body"
  jq -e . <"$dest" >/dev/null 2>&1 || fatal "$GH api $* did not answer valid JSON"
}

# expect_complete <file> <jq-count-expr> — cross-check a paginated list against
# the total the API itself reports, where it reports one.
expect_complete() {
  local file="$1" got_expr="$2" total_expr="$3" what="$4"
  local got total
  got="$(jq -s "$got_expr" <"$file")"
  total="$(jq -s "$total_expr" <"$file")"
  [ "$total" = "null" ] && return 0
  [ "$got" = "$total" ] || fatal "$what: read $got of $total — pagination truncated"
}

# ---------------------------------------------------------------------------
# Verdict accounting
# ---------------------------------------------------------------------------

BROKEN=0
BLIND=0
declare -a LINES=()

# The human report goes to fd 3. Under --json, fd 3 is stderr, so stdout
# carries the JSON document ALONE — an advertised `--json` whose output cannot
# be parsed is worse than none, because the caller's `jq` fails in a way that
# looks like the preflight crashed.
exec 3>&1
[ "$JSON_OUT" = 1 ] && exec 3>&2

agree()    { LINES+=("ok|$1|$2"); printf '  \033[32m✓\033[0m %s — %s\n' "$1" "$2" >&3; }
disagree() { LINES+=("broken|$1|$2"); BROKEN=$((BROKEN + 1)); printf '  \033[31m✗\033[0m %s — %s\n' "$1" "$2" >&3; }

# blind marks ONE agreement unknowable and carries on with the others.
#
# `fatal` is for what makes the whole run impossible; a credential this script
# was not given blinds exactly one agreement, and aborting there would hide
# every disagreement after it — the operator would fix one thing per run. The
# exit code still refuses to call the result a pass.
blind()    { LINES+=("blind|$1|$2"); BLIND=$((BLIND + 1));  printf '  \033[33m?\033[0m %s — BLIND SPOT: %s\n' "$1" "$2" >&3; }

# ---------------------------------------------------------------------------
# Reads
# ---------------------------------------------------------------------------

need_tool jq
need_tool "$GH"

printf '→ %s\n' "$REPO" >&3

api "$WORK/repo" "repos/$REPO"
DEFAULT_BRANCH="$(jq -r '.default_branch // empty' <"$WORK/repo")"
[ -n "$DEFAULT_BRANCH" ] || fatal "repos/$REPO carries no default_branch"

# Rulesets: the union of every ACTIVE branch ruleset THAT GATES THE DEFAULT
# BRANCH, since required checks accumulate across them. An `evaluate`/
# `disabled` ruleset requires nothing, and neither does one scoped to
# `refs/heads/release/*` — dependency PRs target the default branch, so a check
# required only elsewhere is a verdict nobody waits for where it matters.
api "$WORK/rulesets" "repos/$REPO/rulesets"
: >"$WORK/required"
: >"$WORK/bypass"

# ruleset_covers_default <file> — does this ruleset's ref_name condition select
# the default branch? `~ALL` and `~DEFAULT_BRANCH` are GitHub's own tokens; the
# rest are fnmatch patterns, decided by the shell rather than re-implemented in
# jq. An absent `conditions` means the ruleset is unscoped, hence covering.
ruleset_covers_default() {
  local file="$1" verdict pat
  verdict="$(jq -r --arg b "refs/heads/$DEFAULT_BRANCH" '
    (.conditions.ref_name // null) as $c
    | if $c == null then "yes"
      elif (($c.exclude // []) | any(. == $b or . == "~DEFAULT_BRANCH")) then "no"
      elif (($c.include // []) | any(. == "~ALL" or . == "~DEFAULT_BRANCH" or . == $b)) then "yes"
      else "patterns" end' <"$file")"
  [ "$verdict" = "yes" ] && return 0
  [ "$verdict" = "no" ] && return 1
  while read -r pat; do
    [ -n "$pat" ] || continue
    # shellcheck disable=SC2254  # the pattern is the datum being matched
    case "refs/heads/$DEFAULT_BRANCH" in $pat) return 0 ;; esac
  done < <(jq -r '.conditions.ref_name.include[]?' <"$file")
  return 1
}

while read -r rid; do
  [ -n "$rid" ] || continue
  api "$WORK/ruleset-$rid" "repos/$REPO/rulesets/$rid"
  ruleset_covers_default "$WORK/ruleset-$rid" || continue
  jq -r '[.rules[]? | select(.type=="required_status_checks")
          | .parameters.required_status_checks[]?
          | [.context, (.integration_id|tostring)] | @tsv] | .[]' \
    <"$WORK/ruleset-$rid" >>"$WORK/required"
  jq -r '[.bypass_actors[]? | [(.actor_type|tostring), (.actor_id|tostring), (.bypass_mode|tostring)] | @tsv] | .[]' \
    <"$WORK/ruleset-$rid" >>"$WORK/bypass"
done < <(jq -r '.[] | select(.target=="branch") | select(.enforcement=="active") | .id' <"$WORK/rulesets")

REQUIRED_COUNT="$(wc -l <"$WORK/required" | tr -d ' ')"

# The integration: what iterion actually tells the bots at launch.
ITERION_ARGV="${AUTO_MAINT_ITERION:-iterion remote forge repo-bots --json}"
# shellcheck disable=SC2086
if ! $ITERION_ARGV >"$WORK/repobots" 2>"$WORK/repobots.err"; then
  fatal "the repo-bots read ($ITERION_ARGV) did not answer: $(tr '\n' ' ' <"$WORK/repobots.err")"
fi
jq -e . <"$WORK/repobots" >/dev/null 2>&1 || fatal "the repo-bots read did not answer valid JSON"

jq --arg r "$REPO" '[.integrations[]? | select(.repo_full_name == $r)]' \
  <"$WORK/repobots" >"$WORK/integrations"
INTEG_COUNT="$(jq 'length' <"$WORK/integrations")"

# The org's App installations, read once: agreements 3, 4, 5 and 6 all decide
# against them.
api "$WORK/installs" "orgs/$ORG/installations?per_page=100"
expect_complete "$WORK/installs" \
  '[.[].installations[]?]|length' '(.[0].total_count // null)' "orgs/$ORG/installations"

# The Renovate installation. Preferring a repo-scoped install over an org-wide
# one is deliberate — the org carries both, a hosted `renovate` on all repos and
# a self-hosted `socialgouv-renovate` on selected ones — but a PREFERENCE is not
# an answer when two candidates share the same scope. There the pick would be
# decided by the order the API happened to return, so the script refuses instead
# of guessing, and `id` breaks the remaining tie so the refusal names the same
# pair every run.
jq -s '[.[].installations[]?] | map(select(.app_slug | test("renovate"; "i")))
       | sort_by([(.repository_selection == "all"), .id])' <"$WORK/installs" >"$WORK/ren-all.json"
jq '(map(select(.repository_selection != "all"))) as $scoped
    | if ($scoped | length) > 0 then $scoped else . end' <"$WORK/ren-all.json" >"$WORK/ren.json"

REN_COUNT="$(jq 'length' <"$WORK/ren.json")"
REN=""; REN_SLUG=""; REN_ID=""
if [ "$REN_COUNT" -gt 0 ]; then
  REN="$(jq -c '.[0]' <"$WORK/ren.json")"
  REN_SLUG="$(printf '%s' "$REN" | jq -r '.app_slug')"
  REN_ID="$(printf '%s' "$REN" | jq -r '.id')"
fi

# app_is_live <installation-json> <slug> <agreement> — a SUSPENDED installation
# holds every permission it ever held and does nothing with them. Checked on
# both Apps, because a guarantee honoured on one of two sites is the defect
# this whole preflight exists to catch.
app_is_live() {
  local inst="$1" slug="$2" agreement="$3" susp
  susp="$(printf '%s' "$inst" | jq -r '.suspended_at // ""')"
  [ -z "$susp" ] && return 0
  disagree "$agreement" "$slug is SUSPENDED since $susp — it holds its permissions and uses none of them"
  return 1
}

# installation_covers_repo <installation-json> <slug> <id> <agreement>
#
# Shared by BOTH Apps on purpose. The first version of this script asked the
# question of the Renovate App only, and an `iterion-forge-core` scoped away
# from the repository would have passed every permission check while every
# alignment push was refused. Two sites building the same guarantee want one
# helper, or the second drifts.
installation_covers_repo() {
  local inst="$1" slug="$2" id="$3" agreement="$4" sel n_repos
  sel="$(printf '%s' "$inst" | jq -r '.repository_selection')"
  if [ "$sel" = "all" ]; then
    agree "$agreement" "$slug is installed on ALL repositories of $ORG"
    return 0
  fi
  if [ -z "${AUTO_MAINT_PAT:-}" ]; then
    blind "$agreement" "installation $id ($slug) is repository_selection=$sel, and listing its repositories needs a classic PAT: set AUTO_MAINT_PAT (read:user scope). Refusing to assume $REPO is covered."
    return 0
  fi
  if ! GH_TOKEN="$AUTO_MAINT_PAT" "$GH" api --paginate "user/installations/$id/repositories?per_page=100" \
       --jq '.repositories[].full_name' >"$WORK/install-repos-$id" 2>"$WORK/install-repos-$id.err"; then
    fatal "listing the repositories of installation $id failed: $(tr '\n' ' ' <"$WORK/install-repos-$id.err")"
  fi
  n_repos="$(wc -l <"$WORK/install-repos-$id" | tr -d ' ')"
  if grep -qxF "$REPO" "$WORK/install-repos-$id"; then
    agree "$agreement" "$REPO is in the $slug installation ($n_repos repos, selection=$sel)"
  else
    disagree "$agreement" "$slug is repository_selection=$sel and $REPO is NOT among its $n_repos repositories"
  fi
}

# ---------------------------------------------------------------------------
# Agreement 1 — every context a bot POSTS is a context the ruleset REQUIRES
#
# The reverse direction is agreement 7. Both are needed: a verdict nobody waits
# for is advisory, and a required name nobody posts is a PR that never merges.
# ---------------------------------------------------------------------------

# bot_default_gate_context <bot-id> — the `vars:` default from the catalog, or
# empty when the bot declares no gate context at all (then it is not a gate
# producer and has nothing to agree about).
bot_default_gate_context() {
  local bot="$1" file="$BOTS_DIR/$1/main.bot"
  [ -f "$file" ] || fatal "bot $bot is enabled on $REPO but $file is not in the catalog at $BOTS_DIR"
  sed -n 's/^[[:space:]]*gate_context:[[:space:]]*string[^=]*=[[:space:]]*"\([^"]*\)".*/\1/p' "$file" | head -1
}

: >"$WORK/posted"
if [ "$INTEG_COUNT" = "0" ]; then
  disagree "1 gate context" "no iterion integration covers $REPO — nothing posts a verdict"
else
  while read -r integ_id; do
    [ -n "$integ_id" ] || continue
    override="$(jq -r --arg i "$integ_id" \
      '.[] | select(.id==$i) | .launch_vars.gate_context // ""' <"$WORK/integrations")"
    while read -r bot; do
      [ -n "$bot" ] || continue
      default="$(bot_default_gate_context "$bot")"
      [ -n "$default" ] || continue   # not a gate producer
      effective="${override:-$default}"
      printf '%s\t%s\n' "$bot" "$effective" >>"$WORK/posted"
    done < <(jq -r --arg i "$integ_id" '.[] | select(.id==$i) | .bot_ids[]?' <"$WORK/integrations")
  done < <(jq -r '.[].id' <"$WORK/integrations")

  if [ ! -s "$WORK/posted" ]; then
    disagree "1 gate context" "$INTEG_COUNT integration(s), but no enabled bot declares a gate_context"
  else
    while IFS=$'\t' read -r bot ctx; do
      if awk -F'\t' -v c="$ctx" '$1 == c { found = 1 } END { exit !found }' "$WORK/required"; then
        agree "1 gate context" "$bot posts '$ctx', and the ruleset requires it"
      else
        disagree "1 gate context" "$bot posts '$ctx', which NO active ruleset requires — its verdict is advisory ($REQUIRED_COUNT required: $(cut -f1 "$WORK/required" | paste -sd, -))"
      fi
    done <"$WORK/posted"
  fi
fi

# ---------------------------------------------------------------------------
# Agreement 2 — the required check authenticates its PRODUCER
#
# A required check pinned by name alone is satisfied by ANY credential able to
# post a commit status on the repository. The loop's premise is that the gate
# proves an adversarial reviewer saw the change; a name does not prove that.
# ---------------------------------------------------------------------------

if [ ! -s "$WORK/posted" ]; then
  disagree "2 authorised producer" "no posted context to authenticate"
else
  while IFS=$'\t' read -r _bot ctx; do
    integ="$(awk -F'\t' -v c="$ctx" '$1 == c { print $2; exit }' "$WORK/required")"
    case "$integ" in
      ""|null) disagree "2 authorised producer" "required check '$ctx' carries no integration_id — any credential that can post a status satisfies it" ;;
      *)       agree "2 authorised producer" "required check '$ctx' is pinned to App $integ" ;;
    esac
  done <"$WORK/posted"
fi

# Bypass actors are INVENTORY, not an agreement — they are the admin escape
# hatch this project keeps on purpose, so both outcomes are legitimate and
# neither can redden. Printed as a note rather than through agree(), because a
# line that cannot fail should not be counted among lines that can: it inflates
# the report and makes the preflight look more thorough than it is. What the
# reader needs is the list, so that a merge by one of them is never mistaken
# for a conforming merge.
if [ -s "$WORK/bypass" ]; then
  printf '  · inventory — %s bypass actor(s) on the default branch: %s(a merge by one of them did NOT pass the gate)\n' \
    "$(sort -u "$WORK/bypass" | wc -l | tr -d ' ')" \
    "$(sort -u "$WORK/bypass" | awk -F'\t' '{printf "%s:%s(%s) ", $1, $2, $3}')" >&3
else
  printf '  · inventory — no bypass actor on the default branch: every merge went through the gate\n' >&3
fi

# ---------------------------------------------------------------------------
# Agreement 3 — the dependency PRs' author is one Vetty reacts to
#
# The allowlist is the webhook's filter: an author outside it means the guard
# is never invoked, which looks exactly like a repository with no dependency
# PRs to audit.
# ---------------------------------------------------------------------------

VETTY_MANIFEST="$BOTS_DIR/dep-update-guard/manifest.yaml"
if awk -F'\t' '{ print $1 }' <"$WORK/posted" 2>/dev/null | grep -qx 'dep-update-guard'; then
  [ -f "$VETTY_MANIFEST" ] || fatal "dep-update-guard is enabled but $VETTY_MANIFEST is missing"
  # The allowlist is a YAML flow sequence, written today on the line after its
  # key but legal inline — and a `sed` range would then run past it, because
  # sed looks for the end address starting at the line AFTER the start line.
  # awk tracks the bracket depth instead, so both spellings read the same, and
  # the span stops at the closing `]` rather than at the next one in the file.
  # The entries themselves contain brackets (`renovate[bot]`), which is why
  # depth is counted rather than matched.
  ALLOW="$(awk '
    /^[[:space:]]*author_allowlist:/ { inlist = 1 }
    inlist {
      line = $0
      if (!started) {
        if (sub(/^[^[]*\[/, "", line)) { started = 1; depth = 1 } else next
      }
      out = ""
      for (i = 1; i <= length(line); i++) {
        c = substr(line, i, 1)
        if (c == "[") depth++
        else if (c == "]") { depth--; if (depth == 0) { printf "%s", out; exit } }
        out = out c
      }
      printf "%s", out
    }' "$VETTY_MANIFEST" | tr -d ' "')"
  [ -n "$ALLOW" ] || fatal "could not read author_allowlist from $VETTY_MANIFEST"

  api_bounded "$WORK/prs" "repos/$REPO/pulls?state=all&per_page=$PR_SAMPLE&sort=created&direction=desc"
  jq -r '.[] | [.number, (.user.login // ""), (.user.type // "")] | @tsv' <"$WORK/prs" >"$WORK/pr-authors"

  # matches_allowlist <login> — "*" is a SUFFIX wildcard, the form the manifest
  # documents: a self-hosted Renovate App is renamed (acme-renovate[bot]).
  #
  # `set -f` is not decoration: the entries are bracket EXPRESSIONS to the
  # glob engine (`dependabot[bot]`), so an unquoted split with globbing on
  # replaces them with whatever filenames happen to match in the current
  # directory. Measured — from a directory holding files named `xrenovateb`
  # and `ydependabott`, the allowlist became those two names and the preflight
  # reported the real Renovate App as unmatched. A verdict must not depend on
  # where the operator was standing.
  matches_allowlist() {
    local login="$1" pat rc=1
    local IFS=','
    set -f
    for pat in $ALLOW; do
      case "$pat" in
        \**) case "$login" in *"${pat#\*}") rc=0 ;; esac ;;
        *)   [ "$login" = "$pat" ] && rc=0 ;;
      esac
      [ "$rc" = 0 ] && break
    done
    set +f
    return "$rc"
  }

  # The author to agree about is DERIVED from what is installed, never guessed
  # from the bots seen on recent PRs. A repository has other bots — a release
  # bot, a tap updater — and calling each of them a disagreement would bury the
  # one that matters under noise the operator cannot act on.
  : >"$WORK/expected-authors"
  [ -n "$REN_SLUG" ] && printf '%s[bot]\tthe installed Renovate App\n' "$REN_SLUG" >>"$WORK/expected-authors"
  if "$GH" api "repos/$REPO/contents/.github/dependabot.yml" --jq '.name' >/dev/null 2>&1; then
    printf 'dependabot[bot]\t.github/dependabot.yml is present\n' >>"$WORK/expected-authors"
  fi

  if [ ! -s "$WORK/expected-authors" ]; then
    disagree "3 author allowlist" "no dependency bot is installed on $ORG for $REPO — nothing will ever open a dependency PR for Vetty to guard"
  else
    while IFS=$'\t' read -r login why; do
      if matches_allowlist "$login"; then
        agree "3 author allowlist" "'$login' ($why) matches the allowlist $ALLOW"
      else
        disagree "3 author allowlist" "'$login' ($why) matches NO entry of $ALLOW — Vetty never sees its PRs"
      fi
    done <"$WORK/expected-authors"

    # Observed, not asserted: an App that has not opened a PR yet is normal on
    # a freshly wired repository, so this reports rather than decides.
    seen_matching=0
    while read -r login; do
      [ -n "$login" ] || continue
      matches_allowlist "$login" && seen_matching=$((seen_matching + 1))
    done < <(awk -F'\t' '$3 == "Bot" { print $2 }' "$WORK/pr-authors" | sort -u)
    if [ "$seen_matching" -gt 0 ]; then
      agree "3 author allowlist" "$seen_matching allowlisted author(s) actually opened PRs in the last $PR_SAMPLE — the filter is proven, not just declared"
    else
      agree "3 author allowlist" "no allowlisted author among the last $PR_SAMPLE PRs — normal on a freshly wired repository, unproven in practice"
    fi
  fi
elif [ "$INTEG_COUNT" = "0" ]; then
  # Saying "nothing to filter" here would read as a clean bill of health on a
  # repository that has no integration at all — agreement 1 already reddened,
  # and this line must not soften it.
  agree "3 author allowlist" "no integration on $REPO, so no allowlist applies — see agreement 1"
else
  agree "3 author allowlist" "dep-update-guard is not enabled on $REPO — the bots wired here do not filter by author"
fi

# ---------------------------------------------------------------------------
# Agreements 4 and 6 — the Renovate App: what it may do, and where
# ---------------------------------------------------------------------------

if [ -z "$REN" ]; then
  disagree "4 renovate permissions" "no installation whose app_slug matches 'renovate' on org $ORG"
elif [ "$REN_COUNT" -gt 1 ]; then
  blind "4 renovate permissions" "$REN_COUNT renovate installations share the same scope on $ORG ($(jq -r 'map("\(.app_slug)#\(.id)") | join(", ")' <"$WORK/ren.json")) — which one opens PRs on $REPO is not decidable from the org listing, and picking the first would let the API's ordering decide the verdict"
elif ! app_is_live "$REN" "$REN_SLUG" "4 renovate permissions"; then
  : # the refusal is already reported; a suspended App's permissions say nothing
else
  # What the config DEMANDS decides what the App must hold — not a fixed list.
  : >"$WORK/demands"
  # Renovate's own resolution order. Probing only the repository root would
  # report "no config" on a repository that has one — SocialGouv's pilots keep
  # theirs under .github/.
  RENOVATE_CFG_PATH=""
  for candidate in renovate.json renovate.json5 .github/renovate.json .github/renovate.json5 \
                   .renovaterc .renovaterc.json .renovaterc.json5; do
    if "$GH" api "repos/$REPO/contents/$candidate" --jq '.content' >"$WORK/renovate.b64" 2>/dev/null; then
      RENOVATE_CFG_PATH="$candidate"
      break
    fi
  done
  if [ -n "$RENOVATE_CFG_PATH" ]; then
    base64 -d <"$WORK/renovate.b64" >"$WORK/renovate.cfg" 2>/dev/null \
      || fatal "the renovate config at $RENOVATE_CFG_PATH on $REPO is not decodable base64"
    # GitHub answers `content: ""` for a blob over 1 MB. Decoding nothing
    # yields no demand, and no demand would read as "nothing to disagree
    # about" — the exact shape of a comparison that fails open.
    [ -s "$WORK/renovate.cfg" ] \
      || blind "4 renovate permissions" "$RENOVATE_CFG_PATH came back empty (GitHub withholds contents over 1 MB) — what it demands is unknown, not absent"

    # Literal settings. A setting reached through a preset carries no literal,
    # which is why the preset expansion below exists and why an unexpandable
    # `extends` becomes a blind spot rather than a silence.
    grep -q 'pinGitHubActionDigests' "$WORK/renovate.cfg" && printf 'workflows\twrite\thelpers:pinGitHubActionDigests rewrites .github/workflows/**\n' >>"$WORK/demands"
    grep -q 'dependencyDashboard' "$WORK/renovate.cfg" && printf 'issues\twrite\tthe dependency dashboard is an issue\n' >>"$WORK/demands"
    grep -qE 'vulnerabilityAlerts|osvVulnerabilityAlerts' "$WORK/renovate.cfg" && printf 'vulnerability_alerts\tread\tthe CVE exemption reads the repo alerts\n' >>"$WORK/demands"

    # Presets, expanded from renovatebot/renovate's own
    # lib/config/presets/internal/config.preset.ts: `config:recommended`
    # extends `:dependencyDashboard`, and `config:best-practices` extends
    # `config:recommended` plus `helpers:pinGitHubActionDigests`. Without this,
    # a config whose entire body is `{"extends":["config:best-practices"]}`
    # demands two permissions and the script sees none.
    grep -qE '"(config:recommended|config:best-practices)"' "$WORK/renovate.cfg" \
      && printf 'issues\twrite\tconfig:recommended extends :dependencyDashboard\n' >>"$WORK/demands"
    grep -q '"config:best-practices"' "$WORK/renovate.cfg" \
      && printf 'workflows\twrite\tconfig:best-practices extends helpers:pinGitHubActionDigests\n' >>"$WORK/demands"
    sort -u "$WORK/demands" -o "$WORK/demands"
  else
    disagree "4 renovate permissions" "$REPO carries no renovate config at any of the paths Renovate reads — Renovate has nothing to obey"
  fi
  if [ -s "$WORK/demands" ]; then
    while IFS=$'\t' read -r perm level why; do
      have="$(printf '%s' "$REN" | jq -r --arg p "$perm" '.permissions[$p] // "absent"')"
      if [ "$have" = "$level" ] || { [ "$level" = "read" ] && [ "$have" = "write" ]; }; then
        agree "4 renovate permissions" "$REN_SLUG holds $perm:$have, and $RENOVATE_CFG_PATH demands $perm:$level ($why)"
      else
        disagree "4 renovate permissions" "$RENOVATE_CFG_PATH demands $perm:$level ($why) but $REN_SLUG holds $perm:$have"
      fi
    done <"$WORK/demands"
  fi

  # The bound on the preset expansion above, and the reason it is not one more
  # spelling to widen.
  #
  # A guard that enumerates spellings never converges: the next config expresses
  # the same demand through a preset nobody listed. So the expansion is a
  # CONVENIENCE and this is the guarantee — and it is self-limiting, which is
  # what keeps it from being permanent noise. The three permissions tracked here
  # are the only ones any preset could demand; if the App already holds all
  # three, no unexpandable preset can create a disagreement and there is nothing
  # to be blind about. Only when one is missing does an unresolved `extends`
  # mean "this might be the preset that needed it".
  if [ -n "$RENOVATE_CFG_PATH" ] && [ -s "$WORK/renovate.cfg" ]; then
    missing=""
    for pair in "workflows:write" "issues:write" "vulnerability_alerts:read"; do
      perm="${pair%%:*}"; level="${pair##*:}"
      have="$(printf '%s' "$REN" | jq -r --arg p "$perm" '.permissions[$p] // "absent"')"
      if [ "$have" != "$level" ] && ! { [ "$level" = "read" ] && [ "$have" = "write" ]; }; then
        missing="$missing $perm"
      fi
    done
    if [ -n "$missing" ]; then
      unknown="$(grep -oE '"[a-zA-Z0-9:_-]+:[a-zA-Z0-9:_-]+"' "$WORK/renovate.cfg" \
                 | tr -d '"' | sort -u \
                 | grep -vxE 'config:recommended|config:best-practices|helpers:pinGitHubActionDigests' \
                 | paste -sd, - || true)"
      [ -n "$unknown" ] && blind "4 renovate permissions" "$REN_SLUG is missing$missing, and $RENOVATE_CFG_PATH extends presets this script cannot expand ($unknown) — one of them may be what demands it"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Agreement 5 — the DELIVERY App, plus the secrets the workflow consumes
#
# A preflight that checks only the Renovate App passes while the App that
# delivers the alignment cannot touch the very files Renovate bumps.
# ---------------------------------------------------------------------------

DELIVERY_SLUG="${AUTO_MAINT_DELIVERY_APP:-iterion-forge-core}"
DEL="$(jq -s --arg s "$DELIVERY_SLUG" '[.[].installations[]?] | map(select(.app_slug == $s)) | .[0] // empty' <"$WORK/installs")"
if [ -z "$DEL" ]; then
  disagree "5 delivery app" "no installation with app_slug '$DELIVERY_SLUG' on org $ORG — nothing delivers the alignment"
elif app_is_live "$DEL" "$DELIVERY_SLUG" "5 delivery app"; then
  # The alignment must be able to push whatever Renovate may bump. Every
  # write the Renovate App holds on file content is a path the alignment can
  # be asked to touch.
  for perm in contents workflows; do
    del_has="$(printf '%s' "$DEL" | jq -r --arg p "$perm" '.permissions[$p] // "absent"')"
    if [ -z "${REN:-}" ]; then
      blind "5 delivery app" "what Renovate may bump is unknown (no usable renovate installation), so whether $DELIVERY_SLUG's $perm:$del_has is enough cannot be decided"
      continue
    fi
    ren_has="$(printf '%s' "$REN" | jq -r --arg p "$perm" '.permissions[$p] // "absent"')"
    if [ "$ren_has" != "write" ]; then
      agree "5 delivery app" "$DELIVERY_SLUG needs no $perm:write — $REN_SLUG holds $perm:$ren_has and does not bump those paths"
    elif [ "$del_has" = "write" ]; then
      agree "5 delivery app" "$DELIVERY_SLUG holds $perm:write, matching what Renovate may bump"
    else
      disagree "5 delivery app" "Renovate may bump $perm but $DELIVERY_SLUG holds $perm:$del_has — the alignment push is refused for a reason unrelated to the bump"
    fi
  done
  # The same question agreement 6 asks of Renovate. An App scoped away from
  # this repository delivers nothing, whatever its permissions say.
  installation_covers_repo "$DEL" "$DELIVERY_SLUG" \
    "$(printf '%s' "$DEL" | jq -r '.id')" "5 delivery app"
fi

# The workflow's own inputs. Secret NAMES only — this never reads a value.
api "$WORK/secrets" "repos/$REPO/actions/secrets?per_page=100"
expect_complete "$WORK/secrets" '[.[].secrets[]?]|length' '(.[0].total_count // null)' "repos/$REPO/actions/secrets"
for s in RENOVATE_APP_ID RENOVATE_APP_PRIVATE_KEY; do
  if jq -se --arg n "$s" '[.[].secrets[]?.name] | index($n) != null' <"$WORK/secrets" >/dev/null; then
    agree "5 delivery app" "secret $s exists on $REPO"
  else
    disagree "5 delivery app" "secret $s is absent from $REPO — the Renovate workflow cannot mint its token"
  fi
done

# ---------------------------------------------------------------------------
# Agreement 6 — the Renovate App is installed ON THIS REPOSITORY
#
# A `selected` installation that does not include this repo is a Renovate that
# will never open a PR here, and every other agreement would still hold. The
# read that settles it needs a credential an OAuth token cannot provide: name
# which one rather than assume the repository is covered.
# ---------------------------------------------------------------------------

if [ -z "${REN:-}" ]; then
  disagree "6 installation covers the repo" "no renovate installation on org $ORG to cover $REPO"
else
  installation_covers_repo "$REN" "$REN_SLUG" "$REN_ID" "6 installation covers the repo"
fi

# ---------------------------------------------------------------------------
# Agreement 7 — no required gate converges without EXECUTION
#
# The arbitration of this epic: a PR that merges unattended must have had its
# gates run on ITS head. The preflight half of that is falsifiable here — a
# required context that has appeared on none of the sampled PRs is produced by
# nothing, and a PR waiting on it never merges. On a repository that arms
# auto-merge, that is a configuration which cannot converge, so it must refuse
# AT SETUP rather than at 3 a.m.
# ---------------------------------------------------------------------------

# Truthiness as the ENGINE reads it, not as English spells it. `launch_vars` is
# a string map server-side and `arm_automerge` is a bool var, so the value goes
# through ir.CoerceVarValue (pkg/dsl/ir/var_value.go): lowercased, trimmed, and
# "true" / "1" / "yes" all arm. A preflight that only recognised "true" would
# call an armed repository unarmed — on the one setting this epic's arbitration
# hangs on.
ARMED="$(jq -r '[.[].launch_vars.arm_automerge // "false" | tostring | ascii_downcase | sub("^\\s+";"") | sub("\\s+$";"")]
                | if (index("true") or index("1") or index("yes")) then "true" else "false" end' \
          <"$WORK/integrations")"

if [ "$REQUIRED_COUNT" = "0" ]; then
  if [ "$ARMED" = "true" ]; then
    disagree "7 every required gate executes" "arm_automerge is on and the ruleset requires NOTHING — 'clean' says nothing and the forge merges an armed PR straight away"
  else
    agree "7 every required gate executes" "no required check, and auto-merge is not armed"
  fi
else
  [ -f "$WORK/prs" ] || api_bounded "$WORK/prs" "repos/$REPO/pulls?state=all&per_page=$PR_SAMPLE&sort=created&direction=desc"
  : >"$WORK/seen"
  while read -r head; do
    [ -n "$head" ] || continue
    # --paginate emits one JSON document per page, so each expression below is
    # written against ONE page: check-runs pages are objects, statuses pages
    # are arrays.
    api "$WORK/cr-$head" "repos/$REPO/commits/$head/check-runs?per_page=100"
    jq -r '.check_runs[]?.name' <"$WORK/cr-$head" >>"$WORK/seen"
    api "$WORK/st-$head" "repos/$REPO/commits/$head/statuses?per_page=100"
    jq -r '.[]?.context' <"$WORK/st-$head" >>"$WORK/seen"
  done < <(jq -r '.[].head.sha' <"$WORK/prs")
  sort -u "$WORK/seen" -o "$WORK/seen"

  while IFS=$'\t' read -r ctx _integ; do
    [ -n "$ctx" ] || continue
    if grep -qxF "$ctx" "$WORK/seen"; then
      agree "7 every required gate executes" "'$ctx' was produced on the sampled PRs"
    elif [ "$ARMED" = "true" ]; then
      disagree "7 every required gate executes" "'$ctx' is REQUIRED, arm_automerge is on, and nothing produced it on the last $PR_SAMPLE PRs — this configuration cannot converge"
    else
      disagree "7 every required gate executes" "'$ctx' is required but nothing produced it on the last $PR_SAMPLE PRs — every PR here blocks forever"
    fi
  done <"$WORK/required"
fi

# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------

if [ "$JSON_OUT" = 1 ]; then
  # `split("|")` on a detail is wrong: a required check named `build | linux`
  # is an ordinary matrix job name, and splitting on every separator drops the
  # rest of the sentence in silence. Only the first two fields are structural.
  printf '%s\n' "${LINES[@]}" | jq -R -s \
    --arg repo "$REPO" --argjson broken "$BROKEN" --argjson blind "$BLIND" '
    {repo: $repo, broken: $broken, blind: $blind,
     agreements: [ split("\n")[] | select(length > 0) | split("|")
                   | {state: .[0], agreement: .[1], detail: (.[2:] | join("|"))} ]}'
fi

# A blind spot outranks a disagreement: with one open, the list of
# disagreements is not known to be the whole list, so the run cannot be
# reported as "these and only these".
if [ "$BLIND" -gt 0 ]; then
  printf '\n? %s: %d agreement(s) UNKNOWN and %d broken — this run cannot say the list is complete.\n' \
    "$REPO" "$BLIND" "$BROKEN" >&3
  exit 2
fi
if [ "$BROKEN" -gt 0 ]; then
  printf '\n✗ %s: %d agreement(s) BROKEN — the unattended loop is not safe to arm here.\n' "$REPO" "$BROKEN" >&3
  exit 1
fi
printf '\n✓ %s: every agreement holds.\n' "$REPO" >&3
