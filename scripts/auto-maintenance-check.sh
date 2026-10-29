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
#   AUTO_MAINT_PUSH_SAMPLE how many recent pushes to the default branch to
#                         classify as conforming or bypass        (default: 20)
#   AUTO_MAINT_DELIVERY_APP the app_slug of iterion's own App     (default: iterion-forge-core)
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
PUSH_SAMPLE="${AUTO_MAINT_PUSH_SAMPLE:-20}"
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

# sample_head_signals — what ran on the sampled pull request heads, and WHO
# posted each status the forge attributes.
#
# Read once, decided on twice: agreement 2 asks whether the App it compares a
# pin to is the App that actually posts, and agreement 7 asks whether a
# required context is produced at all. Two reads of "what happened on these
# heads" would answer differently the day one of them paginated, and they are
# the same sample by construction. Called lazily, so an agreement that settles
# before needing it costs no API traffic.
sample_head_signals() {
  [ -f "$WORK/seen" ] && return 0
  [ -f "$WORK/prs" ] || api_bounded "$WORK/prs" "repos/$REPO/pulls?state=all&per_page=$PR_SAMPLE&sort=created&direction=desc"
  : >"$WORK/seen"
  : >"$WORK/producers"
  while read -r head; do
    [ -n "$head" ] || continue
    # --paginate emits one JSON document per page, so each expression below is
    # written against ONE page: check-runs pages are objects, statuses pages
    # are arrays.
    api "$WORK/cr-$head" "repos/$REPO/commits/$head/check-runs?per_page=100"
    jq -r '.check_runs[]?.name' <"$WORK/cr-$head" >>"$WORK/seen"
    api "$WORK/st-$head" "repos/$REPO/commits/$head/statuses?per_page=100"
    jq -r '.[]?.context' <"$WORK/st-$head" >>"$WORK/seen"
    # An attributed status only. A forge that reports no creator leaves the
    # producer UNOBSERVED, which agreement 2 says out loud rather than reading
    # as agreement.
    jq -r '.[]? | select((.creator.login // "") != "") | [.context, .creator.login] | @tsv' \
      <"$WORK/st-$head" >>"$WORK/producers"
  done < <(jq -r '.[].head.sha' <"$WORK/prs")
  sort -u "$WORK/seen" -o "$WORK/seen"
  sort -u "$WORK/producers" -o "$WORK/producers"
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
  # Three columns, and the third is not decoration. The SAME context can be
  # required by several active rulesets, each with its own pin — GitHub
  # accumulates them — so a requirement is only decidable together with the
  # ruleset it comes from. A reader that kept the first row it met let the
  # order the API happened to return choose the verdict; the source travels
  # WITH the requirement instead.
  jq -r --arg rs "$rid" '[.rules[]? | select(.type=="required_status_checks")
          | .parameters.required_status_checks[]?
          | [.context, (.integration_id|tostring), $rs] | @tsv] | .[]' \
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

# iterion's OWN App, resolved once. Agreement 2 needs the App id it posts a
# verdict as; agreement 5 needs the same installation's permissions. Two
# resolutions of one identity drift — the defect this whole preflight exists
# to catch — so there is one.
DELIVERY_SLUG="${AUTO_MAINT_DELIVERY_APP:-iterion-forge-core}"
DEL="$(jq -s --arg s "$DELIVERY_SLUG" '[.[].installations[]?] | map(select(.app_slug == $s)) | .[0] // empty' <"$WORK/installs")"

# PRODUCER_APP_ID is the number a ruleset must pin to authenticate that
# verdict: `app_id`, NEVER the installation `id`. They are two different
# numbers on the same payload — measured 2026-10-29 on this org,
# iterion-forge-core is installation 160560029 of App 4895793 — and a
# ruleset's `integration_id`, like a `bypass_actors` entry of actor_type
# Integration, is the APP id. Reading `id` here would compare unrelated
# identifiers and call every correctly pinned repository broken.
PRODUCER_APP_ID=""
[ -n "$DEL" ] && PRODUCER_APP_ID="$(printf '%s' "$DEL" | jq -r '.app_id // empty')"

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
        disagree "1 gate context" "$bot posts '$ctx', which NO active ruleset requires — its verdict is advisory ($REQUIRED_COUNT required: $(cut -f1 "$WORK/required" | sort -u | paste -sd, -))"
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
#
# The decision is a TRIPLE — context x producing App x requiring ruleset — and
# dropping any leg answers a different question:
#
#   * the CONTEXT alone is agreement 1's question, who waits for the verdict;
#   * a pin that is merely PRESENT authenticates nothing. `revi/review` pinned
#     to GitHub Actions (15368) while iterion posts it as its own App reads as
#     "pinned, therefore safe" and is in fact a required check that can never
#     go green — a configuration that cannot converge, which this epic's
#     cursor 2 says must be refused at launch rather than met at 3 a.m.;
#   * the RULESET is where the requirement comes from, and there can be
#     several. Two active rulesets requiring `revi/review`, one pinned and one
#     not, mean a status from any credential still satisfies the unpinned one
#     — invisible to a reader that kept one row per context.
# ---------------------------------------------------------------------------

if [ ! -s "$WORK/posted" ]; then
  disagree "2 authorised producer" "no posted context to authenticate"
elif [ -z "$DEL" ]; then
  # The App iterion posts as is the pivot of the comparison, and unknown is
  # not "no disagreement": agreement 5 reddens for the absent installation,
  # and this agreement refuses to call a pin right or wrong without it.
  blind "2 authorised producer" "no installation with app_slug '$DELIVERY_SLUG' on org $ORG — the App iterion posts its verdict as is unknown, so no pin can be compared to it"
elif [ -z "$PRODUCER_APP_ID" ]; then
  blind "2 authorised producer" "the $DELIVERY_SLUG installation carries no app_id — a ruleset pins an APP id and an installation id is a different number, so comparing them would decide on an unrelated identifier"
else
  # Once per posted CONTEXT, not once per bot: a repo deliberately shares one
  # gate context between several bots (agreement 1 is the per-bot question),
  # and repeating an identical verdict per producer inflates the report and
  # the broken count with the same fact said twice.
  while read -r ctx; do
    [ -n "$ctx" ] || continue
    if ! awk -F'\t' -v c="$ctx" '$1 == c { found = 1 } END { exit !found }' "$WORK/required"; then
      disagree "2 authorised producer" "'$ctx' is required by no active ruleset, so it has no producer to authenticate — see agreement 1"
      continue
    fi
    # The App iterion posts as is DECLARED by the installation carrying
    # $DELIVERY_SLUG, and whether that App is the one that actually posted
    # this context is a DIFFERENT question — the one that decides whether the
    # pin comparison means anything at all. Each tenant deployment carries its
    # own `iterion-forge-*` App, so the default slug is right for the core
    # repository and wrong for a pilot; comparing a correct pin to the wrong
    # App would send the operator to edit a ruleset that is already right.
    sample_head_signals
    observed="$(awk -F'\t' -v c="$ctx" '$1 == c { print $2 }' "$WORK/producers" | sort -u | paste -sd, -)"
    producer="$DELIVERY_SLUG, declared by its installation and unobserved on the last $PR_SAMPLE PRs"
    case ",$observed," in
      ",,") ;;
      *",$DELIVERY_SLUG[bot],"*) producer="$DELIVERY_SLUG, observed posting it" ;;
      *) disagree "2 authorised producer" "'$ctx' is posted by $observed, not by $DELIVERY_SLUG — the App this agreement compares the pin to is not the App producing the verdict, so neither answer about the pin would be worth anything (name the right one with AUTO_MAINT_DELIVERY_APP)"
         continue ;;
    esac
    # EVERY row that requires this context, never the first one found.
    while IFS=$'\t' read -r _c integ rs; do
      case "$integ" in
        ""|null)
          disagree "2 authorised producer" "ruleset $rs requires '$ctx', which carries no integration_id — any credential that can post a status satisfies it" ;;
        "$PRODUCER_APP_ID")
          agree "2 authorised producer" "ruleset $rs pins '$ctx' to App $integ, the App iterion posts as ($producer)" ;;
        *)
          disagree "2 authorised producer" "producing App != pinned App: ruleset $rs pins '$ctx' to App $integ, but iterion posts it as App $PRODUCER_APP_ID ($DELIVERY_SLUG) — the pinned App never posts this context, so the check can never go green" ;;
      esac
    done < <(awk -F'\t' -v c="$ctx" '$1 == c' "$WORK/required")
  done < <(cut -f2 "$WORK/posted" | sort -u)
fi

# Bypass actors are INVENTORY: who MAY bypass. Their presence is not a
# disagreement — it is the admin escape hatch this project keeps on purpose —
# so this stays a note rather than an agree() line, because a line that cannot
# fail should not be counted among lines that can.
#
# What it must NOT do is answer the reader's actual question. "No bypass actor,
# therefore every merge went through the gate" was printed here about pushes
# nobody had read; who DID bypass is agreement 8, which reads them.
if [ -s "$WORK/bypass" ]; then
  printf '  · inventory — %s bypass actor(s) on the default branch: %s(a merge by one of them did NOT pass the gate; agreement 8 says which)\n' \
    "$(sort -u "$WORK/bypass" | wc -l | tr -d ' ')" \
    "$(sort -u "$WORK/bypass" | awk -F'\t' '{printf "%s:%s(%s) ", $1, $2, $3}')" >&3
else
  printf '  · inventory — no bypass actor is listed on the default branch; agreement 8 says whether any push bypassed it anyway\n' >&3
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

# DELIVERY_SLUG and DEL are resolved once, next to the installations read:
# agreement 2 decides against the same identity.
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
  sample_head_signals

  while IFS=$'\t' read -r ctx _integ _rs; do
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
# Agreement 8 — a conforming merge is told apart from a bypass merge
#
# The inventory above says who MAY bypass. It cannot say who DID, and the line
# printed in its place ("every merge went through the gate", whenever the list
# came back empty) was a statement about pushes nobody had read.
#
# GitHub evaluates every push to a ruleset-covered ref and keeps its own
# verdict: `rulesets/rule-suites` answers pass | fail | bypass with the actor
# that pushed. That is the measurement, so this reads it rather than inferring
# a merge actor from a pull request — a queue merge, a squash and a direct
# push are three different pushes there and one shape here.
#
# What can redden is a bypass NO inventory entry can explain, decided by
# SHAPE: an App bypassed while the ruleset lists no Integration, or a human
# bypassed while it lists no role-shaped actor. Shape is as far as this read
# honestly goes. A bypass by a repository admin is reported as the ACTOR and
# never as the role id, because rule-suites names the user who pushed while
# the ruleset names the role that let them, and nothing read here joins the
# two — asserting the join would be the same unfalsifiable line in a new
# costume.
# ---------------------------------------------------------------------------

HAS_INTEGRATION_BYPASS=0
HAS_ACTOR_BYPASS=0
if [ -s "$WORK/bypass" ]; then
  while IFS=$'\t' read -r atype _aid _mode; do
    [ -n "$atype" ] || continue
    case "$atype" in
      Integration) HAS_INTEGRATION_BYPASS=1 ;;
      *)           HAS_ACTOR_BYPASS=1 ;;
    esac
  done < <(sort -u "$WORK/bypass")
fi

# Not through api(): an operator without admin on the repository gets 403
# here, and that blinds ONE agreement rather than aborting the run — the same
# reason blind() exists at all. The bound is the point, so no --paginate.
if ! "$GH" api "repos/$REPO/rulesets/rule-suites?ref=refs/heads/$DEFAULT_BRANCH&per_page=$PUSH_SAMPLE" \
     >"$WORK/suites" 2>"$WORK/suites.err"; then
  blind "8 conforming vs bypass merge" "the rule-suite history of refs/heads/$DEFAULT_BRANCH is unreadable (it needs admin on the repository): $(tr '\n' ' ' <"$WORK/suites.err") — which pushes bypassed the gate is UNKNOWN, not none"
elif ! jq -e 'type == "array"' <"$WORK/suites" >/dev/null 2>&1; then
  # Valid JSON is not enough: `gh` prints an error OBJECT with exit 0 on some
  # paths, and `jq 'length'` on an object counts its keys while `.[]` walks
  # its values — every count below would be a number about the wrong thing.
  fatal "repos/$REPO/rulesets/rule-suites did not answer a JSON array of rule suites (got $(jq -r 'type' <"$WORK/suites" 2>/dev/null || echo "unparseable"))"
else
  SUITES_TOTAL="$(jq 'length' <"$WORK/suites")"
  SUITES_PASS="$(jq '[.[] | select(.result=="pass")] | length' <"$WORK/suites")"
  SUITES_FAIL="$(jq '[.[] | select(.result=="fail")] | length' <"$WORK/suites")"
  SUITES_BYPASS="$(jq '[.[] | select(.result=="bypass")] | length' <"$WORK/suites")"
  if [ "$SUITES_TOTAL" = "0" ]; then
    # Said plainly rather than dressed as a pass: an empty window proves
    # nothing in either direction, and this is the one honest wording for it.
    agree "8 conforming vs bypass merge" "no push to refs/heads/$DEFAULT_BRANCH in the window rule-suites covers — nothing is known to have bypassed the gate, and nothing is known to have passed it"
  elif [ "$SUITES_BYPASS" = "0" ]; then
    agree "8 conforming vs bypass merge" "$SUITES_PASS of the last $SUITES_TOTAL push(es) to $DEFAULT_BRANCH passed the ruleset, $SUITES_FAIL were refused by it, and none bypassed it"
  else
    # One line per distinct actor, with the count and the revisions. No
    # ordering is claimed: rule-suites' order is not a fact this script reads
    # a key for, so "the most recent bypass" is not printed at all.
    while IFS=$'\t' read -r actor n shas; do
      [ -n "$actor" ] || continue
      case "$actor" in
        *'[bot]')
          slug="${actor%'[bot]'}"
          app_id="$(jq -s -r --arg s "$slug" '[.[].installations[]?] | map(select(.app_slug == $s)) | (.[0].app_id // empty) | tostring' <"$WORK/installs")"
          if [ -z "$app_id" ] || [ "$app_id" = "null" ]; then
            disagree "8 conforming vs bypass merge" "$actor bypassed the ruleset on $n of the last $SUITES_TOTAL push(es) to $DEFAULT_BRANCH ($shas) and no installation on $ORG carries app_slug '$slug' — the bypass list cannot be matched to it, so these merges are unexplained"
          elif awk -F'\t' -v a="$app_id" '$1 == "Integration" && $2 == a { found = 1 } END { exit !found }' "$WORK/bypass"; then
            agree "8 conforming vs bypass merge" "$actor (App $app_id) bypassed the ruleset on $n of the last $SUITES_TOTAL push(es) to $DEFAULT_BRANCH ($shas) — sanctioned: the ruleset lists Integration $app_id, and these merges did NOT pass the gate"
          else
            disagree "8 conforming vs bypass merge" "$actor (App $app_id) bypassed the ruleset on $n of the last $SUITES_TOTAL push(es) to $DEFAULT_BRANCH ($shas), and the bypass list does not name Integration $app_id — an App is merging past the gate without being allowed to"
          fi ;;
        *)
          if [ "$HAS_ACTOR_BYPASS" = 1 ]; then
            agree "8 conforming vs bypass merge" "$actor bypassed the ruleset on $n of the last $SUITES_TOTAL push(es) to $DEFAULT_BRANCH ($shas) — the ruleset sanctions a role-shaped escape hatch ($(sort -u "$WORK/bypass" | awk -F'\t' '$1 != "Integration" {printf "%s:%s ", $1, $2}')), and these merges did NOT pass the gate"
          else
            disagree "8 conforming vs bypass merge" "$actor bypassed the ruleset on $n of the last $SUITES_TOTAL push(es) to $DEFAULT_BRANCH ($shas), and the bypass list names no role-shaped actor that could explain it — these merges went past a gate nobody was allowed to skip"
          fi ;;
      esac
    done < <(jq -r '[.[] | select(.result=="bypass")] | group_by(.actor_name)[]
                    | ([.[] | ((.after_sha // "?")[0:8])]) as $shas
                    | [ (.[0].actor_name // "?"), (length|tostring),
                        (($shas[0:3] | join(", ")) + (if ($shas|length) > 3 then ", +\(($shas|length)-3) more" else "" end)) ]
                    | @tsv' <"$WORK/suites")
    if [ "$SUITES_PASS" != "0" ]; then
      agree "8 conforming vs bypass merge" "$SUITES_PASS of the same $SUITES_TOTAL push(es) DID pass the ruleset — those are the conforming merges"
    fi
  fi
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
