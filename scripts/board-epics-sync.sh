#!/usr/bin/env bash
# board-epics-sync — keep the GitHub board's `Epic` field true to the sub-issue
# hierarchy, and report anything the two disagree on.
#
# The board carries the same grouping twice, on purpose:
#
#   * SUB-ISSUES are the AUTHORITY. `addSubIssue` is native, it survives off
#     the board, and it gives each epic a progress rollup in the repo itself.
#   * The `Epic` single-select field is a PROJECTION of that parent. It exists
#     because the views need it: a board's columns accept a single-select but
#     never "Parent issue", a filter can match a field but not a parent, and
#     GitHub's Board+swimlane-by-parent rendering drops cards silently
#     (community #193324). A read model over the hierarchy — never a second
#     source of truth.
#
# Two representations drift unless something re-derives one from the other.
# That is this script. It is idempotent: a second consecutive run reports no
# divergence and writes nothing.
#
#   scripts/board-epics-sync.sh            # report only (default)
#   scripts/board-epics-sync.sh --apply    # report, then fix the projection
#
# Closed items generally carry the field WITHOUT a parent: sub-issues are
# attached to open work only, so the progress bar means "what is left" rather
# than a truncated history. Those are reported as `field-only`, which is the
# expected state, not a defect.

set -euo pipefail

ORG=SocialGouv
REPO=iterion
PROJECT_NUMBER=203

APPLY=0
[[ "${1:-}" == "--apply" ]] && APPLY=1

command -v gh >/dev/null || { echo "gh is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# ---------------------------------------------------------------- project ids

read -r PROJECT_ID EPIC_FIELD_ID < <(
  gh api graphql -f query='
    query($org:String!,$num:Int!){
      organization(login:$org){ projectV2(number:$num){
        id
        field(name:"Epic"){ ... on ProjectV2SingleSelectField { id } }
      } }
    }' -F org="$ORG" -F num="$PROJECT_NUMBER" \
    --jq '.data.organization.projectV2 | "\(.id) \(.field.id)"'
)
[[ -n "$PROJECT_ID" && -n "$EPIC_FIELD_ID" ]] || {
  echo "could not resolve the project or its Epic field" >&2; exit 1; }

gh api graphql -f query='
  query($org:String!,$num:Int!){
    organization(login:$org){ projectV2(number:$num){
      field(name:"Epic"){ ... on ProjectV2SingleSelectField { options { id name } } }
    } }
  }' -F org="$ORG" -F num="$PROJECT_NUMBER" \
  --jq '.data.organization.projectV2.field.options[] | "\(.name)\t\(.id)"' > "$work/options.tsv"

# ------------------------------------------------------------- board contents

cat > "$work/items.graphql" <<'GQL'
query($org: String!, $num: Int!, $endCursor: String) {
  organization(login: $org) {
    projectV2(number: $num) {
      items(first: 100, after: $endCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          content {
            ... on Issue {
              number
              labels(first: 20) { nodes { name } }
              parent { number title }
            }
          }
          fieldValues(first: 30) {
            nodes {
              ... on ProjectV2ItemFieldSingleSelectValue {
                name
                field { ... on ProjectV2FieldCommon { name } }
              }
            }
          }
        }
      }
    }
  }
}
GQL

gh api graphql --paginate -F org="$ORG" -F num="$PROJECT_NUMBER" \
  -F query=@"$work/items.graphql" > "$work/raw.json"

jq -s '
  map(.data.organization.projectV2.items.nodes) | add | map(select(.content.number != null)) |
  map({
    item:   .id,
    num:    .content.number,
    isEpic: ([.content.labels.nodes[]?.name] | index("epic") != null),
    parent: .content.parent.number,
    epic:   ([.fieldValues.nodes[]? | select(.field.name == "Epic") | .name][0])
  })' "$work/raw.json" > "$work/items.json"

# An epic issue declares which option it owns: the option whose name its own
# Epic field holds. That keeps the mapping in the board, not in this script —
# add an epic without touching a line here.
jq -r '.[] | select(.isEpic) | "\(.num)\t\(.epic // "")"' "$work/items.json" > "$work/epicmap.tsv"

echo "board: $(jq length "$work/items.json") items, $(wc -l < "$work/epicmap.tsv") epics"
echo

# --------------------------------------------------------------------- checks

divergent=0
missing=0
unmapped=0

while IFS=$'\t' read -r num opt; do
  if [[ -z "$opt" ]]; then
    echo "UNMAPPED  #$num is labelled 'epic' but carries no Epic value of its own"
    unmapped=$((unmapped + 1))
  fi
done < "$work/epicmap.tsv"

: > "$work/fix.tsv"
while IFS=$'\t' read -r item num parent epic; do
  if [[ -z "$epic" || "$epic" == "null" ]]; then
    echo "NO EPIC   #$num carries no Epic value"
    missing=$((missing + 1))
    continue
  fi
  [[ "$parent" == "null" || -z "$parent" ]] && continue

  # The rule holds at any depth: an item's Epic is its PARENT's Epic. When the
  # parent is an epic issue, that is the epic itself; when the parent is an
  # ordinary ticket (a legitimate two-level chain, e.g. #1209 → #1072 → the
  # Connectors epic), it is whatever that ticket carries.
  expected=$(jq -r --argjson p "$parent" '.[] | select(.num == $p) | .epic // ""' "$work/items.json")
  if [[ -z "$expected" || "$expected" == "null" ]]; then
    echo "ORPHAN    #$num has parent #$parent, which carries no Epic (off the board?)"
    missing=$((missing + 1))
    continue
  fi
  if [[ "$epic" != "$expected" ]]; then
    echo "DIVERGENT #$num  parent #$parent says '$expected', field says '$epic'"
    printf '%s\t%s\t%s\n' "$item" "$num" "$expected" >> "$work/fix.tsv"
    divergent=$((divergent + 1))
  fi
done < <(jq -r '.[] | [.item, .num, (.parent // "null"), (.epic // "")] | @tsv' "$work/items.json")

fieldonly=$(jq '[.[] | select(.parent == null and .epic != null and (.isEpic | not))] | length' "$work/items.json")

echo
echo "field-only (no parent — expected for closed work): $fieldonly"
echo "divergent: $divergent   missing an epic: $missing   unmapped epics: $unmapped"

# ---------------------------------------------------------------------- apply

if (( divergent > 0 )) && (( APPLY )); then
  echo
  echo "applying $divergent correction(s) — the parent wins"
  while IFS=$'\t' read -r item num expected; do
    optid=$(awk -F'\t' -v n="$expected" '$1 == n { print $2 }' "$work/options.tsv")
    [[ -n "$optid" ]] || { echo "  #$num: no option named '$expected'" >&2; continue; }
    gh api graphql -f query='
      mutation($p:ID!,$i:ID!,$f:ID!,$o:String!){
        updateProjectV2ItemFieldValue(input:{
          projectId:$p, itemId:$i, fieldId:$f, value:{singleSelectOptionId:$o}
        }){ projectV2Item { id } }
      }' -F p="$PROJECT_ID" -F i="$item" -F f="$EPIC_FIELD_ID" -F o="$optid" >/dev/null
    echo "  #$num -> $expected"
  done < "$work/fix.tsv"
  echo "re-run without --apply: it must report 0 divergent."
fi

if (( divergent > 0 || missing > 0 || unmapped > 0 )); then
  (( APPLY )) || echo
  (( APPLY )) || echo "run with --apply to fix the projection (or attach the missing parents first)"
  exit 1
fi

echo "board is consistent"
