package ciguard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The behaviour of scripts/board-epics-sync.sh: it re-derives the board's
// `Epic` single-select field from the sub-issue hierarchy, and with --apply
// writes the parent's value back. The script talks to GitHub only through
// `gh api graphql`, so the witnesses below run it against a stand-in gh that
// serves a fixture board and records every mutation.

const boardEpicsSyncScript = "../../scripts/board-epics-sync.sh"

// standInBoardGH answers the three queries and the one mutation the script
// sends. It types its flags the way gh does: `-f k=v` binds a string, while
// `-F k=v` binds an integer-looking value as a JSON number, true/false/null as
// JSON, and `@file` as the file's text. GitHub then refuses a non-string bound
// to a variable declared `String!`; gh 2.105.0, measured by hand:
//
//	gh api graphql -f query='query($o:String!){organization(login:$o){id}}' -F o=13616618
//	→ exit 1, "Variable $o of type String! was provided invalid value"
//
// The stand-in reproduces that refusal. A mutation on the item named by
// $BOARD_REFUSE_ITEM is refused too; every accepted mutation appends
// `item<TAB>option` to $BOARD/mutations.tsv.
const standInBoardGH = `#!/usr/bin/env bash
set -euo pipefail
[ "${1:-} ${2:-}" = "api graphql" ] || { echo "unexpected gh call: $*" >&2; exit 2; }
shift 2
vars='{}'
jqexpr=''
while [ $# -gt 0 ]; do
  case "$1" in
    -f)
      k=${2%%=*}; v=${2#*=}; shift 2
      vars=$(jq -c --arg k "$k" --arg v "$v" '.[$k] = $v' <<<"$vars") ;;
    -F)
      k=${2%%=*}; v=${2#*=}; shift 2
      if [[ $v =~ ^([+-]?)([0-9]+)$ ]]; then
        n=$((10#${BASH_REMATCH[2]}))
        [ "${BASH_REMATCH[1]}" = "-" ] && n=-$n
        vars=$(jq -c --arg k "$k" --argjson v "$n" '.[$k] = $v' <<<"$vars")
      elif [ "$v" = true ] || [ "$v" = false ] || [ "$v" = null ]; then
        vars=$(jq -c --arg k "$k" --argjson v "$v" '.[$k] = $v' <<<"$vars")
      elif [[ $v == @* ]]; then
        vars=$(jq -c --arg k "$k" --rawfile v "${v#@}" '.[$k] = $v' <<<"$vars")
      else
        vars=$(jq -c --arg k "$k" --arg v "$v" '.[$k] = $v' <<<"$vars")
      fi ;;
    --jq) jqexpr=$2; shift 2 ;;
    --paginate) shift ;;
    *) echo "unexpected gh argument: $1" >&2; exit 2 ;;
  esac
done
query=$(jq -r '.query' <<<"$vars")
for name in $(jq -r 'del(.query) | to_entries[] | select(.value | type != "string") | .key' <<<"$vars"); do
  if grep -Eq "\\\$${name}[[:space:]]*:[[:space:]]*String!" <<<"$query"; then
    echo "gh: Variable \$${name} of type String! was provided invalid value" >&2
    exit 1
  fi
done
case "$query" in
  *updateProjectV2ItemFieldValue*)
    i=$(jq -r '.i' <<<"$vars"); o=$(jq -r '.o' <<<"$vars")
    if [ "$i" = "${BOARD_REFUSE_ITEM:-}" ]; then
      echo "gh: the update of item $i was refused" >&2
      exit 1
    fi
    printf '%s\t%s\n' "$i" "$o" >> "$BOARD/mutations.tsv"
    resp=$(jq -cn --arg i "$i" '{data: {updateProjectV2ItemFieldValue: {projectV2Item: {id: $i}}}}') ;;
  *"options { id name }"*) resp=$(cat "$BOARD/options.json") ;;
  *"items(first:"*) resp=$(cat "$BOARD/items.json") ;;
  *) resp=$(cat "$BOARD/project.json") ;;
esac
if [ -n "$jqexpr" ]; then
  jq -r "$jqexpr" <<<"$resp"
else
  printf '%s\n' "$resp"
fi
`

// The Epic options of the fixture board: the real names and ids of board 203.
// 13616618 is the one all-digit id the live field carries.
const (
	ciHealthName  = "🩺 CI health"
	ciHealthID    = "13616618"
	cloudName     = "☁️ Cloud plane"
	cloudID       = "7fff2f61"
	brandName     = "🏷️ Brand"
	brandID       = "0228e477"
	ciHealthEpic  = 1410
	cloudEpic     = 1407
	projectNodeID = "PVT_kwDOAx-2Ec4BDrFz"
	epicFieldID   = "PVTSSF_lADOAx-2Ec4BDrFzzg1Lk3s"
)

// boardItem is one card of the fixture board. Parent 0 means no parent, Epic
// "" means the field is unset.
type boardItem struct {
	ID     string
	Num    int
	Epic   string
	Parent int
	IsEpic bool
}

// The JSON shapes the script's GraphQL queries return.
type (
	ghName struct {
		Name string `json:"name"`
	}
	ghParent struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	ghFieldValue struct {
		Name  string `json:"name"`
		Field ghName `json:"field"`
	}
	ghItemContent struct {
		Number int `json:"number"`
		Labels struct {
			Nodes []ghName `json:"nodes"`
		} `json:"labels"`
		Parent *ghParent `json:"parent"`
	}
	ghItem struct {
		ID          string        `json:"id"`
		Content     ghItemContent `json:"content"`
		FieldValues struct {
			Nodes []ghFieldValue `json:"nodes"`
		} `json:"fieldValues"`
	}
)

// epicItems are the two epic issues every fixture board carries, each holding
// its own option.
func epicItems() []boardItem {
	return []boardItem{
		{ID: "PVTI_epic_ci", Num: ciHealthEpic, Epic: ciHealthName, IsEpic: true},
		{ID: "PVTI_epic_cloud", Num: cloudEpic, Epic: cloudName, IsEpic: true},
	}
}

type boardSyncRun struct {
	code      int
	stdout    string
	stderr    string
	mutations []string // "item<TAB>option", in the order they were accepted
}

// runBoardSync runs the script against a board holding items.
func runBoardSync(t *testing.T, items []boardItem, apply bool, refuseItem string) boardSyncRun {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("jq is required by the script and its stand-in gh; it is declared in devbox.json, so run through `devbox run --`: %v", err)
	}
	board := t.TempDir()
	writeJSON := func(name string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(board, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type m = map[string]any
	writeJSON("project.json", m{"data": m{"organization": m{"projectV2": m{
		"id": projectNodeID, "field": m{"id": epicFieldID},
	}}}})
	writeJSON("options.json", m{"data": m{"organization": m{"projectV2": m{"field": m{
		"options": []m{{"id": ciHealthID, "name": ciHealthName}, {"id": cloudID, "name": cloudName}, {"id": brandID, "name": brandName}},
	}}}}})
	nodes := make([]ghItem, 0, len(items))
	for _, it := range items {
		n := ghItem{ID: it.ID, Content: ghItemContent{Number: it.Num}}
		n.Content.Labels.Nodes = []ghName{}
		if it.IsEpic {
			n.Content.Labels.Nodes = append(n.Content.Labels.Nodes, ghName{Name: "epic"})
		}
		if it.Parent != 0 {
			n.Content.Parent = &ghParent{Number: it.Parent, Title: "parent"}
		}
		n.FieldValues.Nodes = []ghFieldValue{{Name: "Todo", Field: ghName{Name: "Status"}}}
		if it.Epic != "" {
			n.FieldValues.Nodes = append(n.FieldValues.Nodes, ghFieldValue{Name: it.Epic, Field: ghName{Name: "Epic"}})
		}
		nodes = append(nodes, n)
	}
	writeJSON("items.json", m{"data": m{"organization": m{"projectV2": m{"items": m{
		"pageInfo": m{"hasNextPage": false, "endCursor": nil},
		"nodes":    nodes,
	}}}}})

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(standInBoardGH), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(boardEpicsSyncScript)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{scriptUnderTest(t, script)}
	if apply {
		args = append(args, "--apply")
	}
	cmd := exec.Command("bash", args...)
	cmd.Env = scriptEnv(t)
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PATH=") {
			cmd.Env[i] = "PATH=" + bin + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
	}
	// The script's own mktemp dir lands in test scratch.
	cmd.Env = append(cmd.Env, "TMPDIR="+t.TempDir(), "BOARD="+board, "BOARD_REFUSE_ITEM="+refuseItem)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	run := boardSyncRun{}
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v\nstderr: %s", script, err, errb.String())
		}
		run.code = exit.ExitCode()
	}
	run.stdout, run.stderr = out.String(), errb.String()
	recorded, err := os.ReadFile(filepath.Join(board, "mutations.tsv"))
	switch {
	case os.IsNotExist(err):
	case err != nil:
		t.Fatal(err)
	default:
		run.mutations = strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
	}
	return run
}

func (r boardSyncRun) String() string {
	return "exit " + strconv.Itoa(r.code) + "\nstdout:\n" + r.stdout + "\nstderr:\n" + r.stderr + "\nmutations: " + strings.Join(r.mutations, " | ")
}

// A correction toward "🩺 CI health" sends its all-digit option id as a
// string, and the correction after it is applied too — in both orders.
func TestBoardEpicsSyncAppliesAnAllDigitOptionID(t *testing.T) {
	toCI := boardItem{ID: "PVTI_to_ci", Num: 3001, Epic: cloudName, Parent: ciHealthEpic}
	toCloud := boardItem{ID: "PVTI_to_cloud", Num: 3002, Epic: ciHealthName, Parent: cloudEpic}
	for name, order := range map[string][]boardItem{
		"all-digit id first": {toCI, toCloud},
		"all-digit id last":  {toCloud, toCI},
	} {
		t.Run(name, func(t *testing.T) {
			r := runBoardSync(t, append(epicItems(), order...), true, "")
			want := make([]string, 0, 2)
			for _, it := range order {
				opt := cloudID
				if it.Parent == ciHealthEpic {
					opt = ciHealthID
				}
				want = append(want, it.ID+"\t"+opt)
			}
			if !slices.Equal(r.mutations, want) {
				t.Fatalf("mutations = %q, want %q\n%s", r.mutations, want, r)
			}
			if !strings.Contains(r.stdout, "#3001 -> "+ciHealthName) || !strings.Contains(r.stdout, "#3002 -> "+cloudName) {
				t.Fatalf("both corrections must be reported as applied\n%s", r)
			}
			if r.code != 1 {
				t.Fatalf("an apply run whose corrections all land exits 1: the proof is the second run\n%s", r)
			}
		})
	}
}

// A ticket with a parent and no Epic value is reported UNSET and written by
// --apply; a ticket with neither stays NO EPIC and is never written.
func TestBoardEpicsSyncFillsAnUnsetEpicFromItsParent(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_unset", Num: 3010, Parent: cloudEpic},
		boardItem{ID: "PVTI_loose", Num: 3011},
	)

	report := runBoardSync(t, items, false, "")
	if !strings.Contains(report.stdout, "UNSET     #3010 carries no Epic value; parent #1407 says '"+cloudName+"'") {
		t.Fatalf("report mode must name the unset ticket and its parent's epic\n%s", report)
	}
	if !strings.Contains(report.stdout, "NO EPIC   #3011 carries no Epic value") {
		t.Fatalf("a ticket without a parent stays NO EPIC\n%s", report)
	}
	if report.code != 1 || len(report.mutations) != 0 {
		t.Fatalf("report mode exits 1 and writes nothing\n%s", report)
	}

	applied := runBoardSync(t, items, true, "")
	if want := []string{"PVTI_unset\t" + cloudID}; !slices.Equal(applied.mutations, want) {
		t.Fatalf("mutations = %q, want %q\n%s", applied.mutations, want, applied)
	}
}

// Without --apply the script reports a divergence and writes nothing.
func TestBoardEpicsSyncReportsWithoutWriting(t *testing.T) {
	r := runBoardSync(t, append(epicItems(), boardItem{ID: "PVTI_to_ci", Num: 3001, Epic: cloudName, Parent: ciHealthEpic}), false, "")
	if !strings.Contains(r.stdout, "DIVERGENT #3001") {
		t.Fatalf("the divergence must be reported\n%s", r)
	}
	if len(r.mutations) != 0 {
		t.Fatalf("report mode must not write\n%s", r)
	}
	if r.code != 1 || !strings.Contains(r.stdout, "run with --apply") {
		t.Fatalf("report mode exits 1 and points at --apply\n%s", r)
	}
}

// A consistent board exits 0, says so, and writes nothing, with or without
// --apply.
func TestBoardEpicsSyncAConsistentBoardWritesNothing(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_ok", Num: 3020, Epic: ciHealthName, Parent: ciHealthEpic},
		boardItem{ID: "PVTI_closed", Num: 3021, Epic: cloudName},
	)
	for _, apply := range []bool{false, true} {
		r := runBoardSync(t, items, apply, "")
		if r.code != 0 || !strings.Contains(r.stdout, "board is consistent") || len(r.mutations) != 0 {
			t.Fatalf("apply=%v: a consistent board exits 0, says so, and writes nothing\n%s", apply, r)
		}
	}
}

// A refused mutation is named on stderr and does not stop the corrections
// after it; the run exits 2, which no run without a refusal returns.
func TestBoardEpicsSyncARefusedCorrectionDoesNotStopTheOthers(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_refused", Num: 3030, Epic: ciHealthName, Parent: cloudEpic},
		boardItem{ID: "PVTI_after", Num: 3031, Epic: cloudName, Parent: ciHealthEpic},
	)
	r := runBoardSync(t, items, true, "PVTI_refused")
	if want := []string{"PVTI_after\t" + ciHealthID}; !slices.Equal(r.mutations, want) {
		t.Fatalf("mutations = %q, want %q\n%s", r.mutations, want, r)
	}
	if !strings.Contains(r.stderr, "#3030: the update to '"+cloudName+"' was refused") {
		t.Fatalf("the refused correction must be named on stderr\n%s", r)
	}
	if !strings.Contains(r.stderr, "1 correction(s) failed") {
		t.Fatalf("the failure count must be printed\n%s", r)
	}
	if r.code != 2 {
		t.Fatalf("a run with a refused correction exits 2\n%s", r)
	}
}

// An epic issue's own value is its ownership declaration. A sub-epic without
// one is reported UNMAPPED only: never UNSET, never written from its parent.
func TestBoardEpicsSyncNeverFillsAnUnmappedSubEpic(t *testing.T) {
	items := append(epicItems(), boardItem{ID: "PVTI_subepic", Num: 3500, Parent: cloudEpic, IsEpic: true})
	for _, apply := range []bool{false, true} {
		r := runBoardSync(t, items, apply, "")
		if !strings.Contains(r.stdout, "UNMAPPED  #3500") {
			t.Fatalf("apply=%v: the sub-epic without a value must be reported UNMAPPED\n%s", apply, r)
		}
		if strings.Contains(r.stdout, "UNSET     #3500") || strings.Contains(r.stdout, "NO EPIC   #3500") {
			t.Fatalf("apply=%v: an epic issue is reported UNMAPPED only\n%s", apply, r)
		}
		if len(r.mutations) != 0 || r.code != 1 {
			t.Fatalf("apply=%v: an unmapped sub-epic exits 1 and is never written\n%s", apply, r)
		}
	}
}

// A sub-epic keeps the option it owns even when its parent epic holds another;
// its own children follow the sub-epic, not the epic above it.
func TestBoardEpicsSyncKeepsASubEpicsOwnOption(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_subepic", Num: 3800, Epic: brandName, Parent: cloudEpic, IsEpic: true},
		boardItem{ID: "PVTI_child_unset", Num: 3801, Parent: 3800},
		boardItem{ID: "PVTI_child_wrong", Num: 3802, Epic: cloudName, Parent: 3800},
	)
	r := runBoardSync(t, items, true, "")
	if want := []string{"PVTI_child_unset\t" + brandID, "PVTI_child_wrong\t" + brandID}; !slices.Equal(r.mutations, want) {
		t.Fatalf("mutations = %q, want %q\n%s", r.mutations, want, r)
	}
}

// An unset leaf under a DIVERGENT mid-ticket gets the mid-ticket's corrected
// Epic in the same pass, not the stale value that pass replaces.
func TestBoardEpicsSyncFillsALeafWithItsParentsCorrectedEpic(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_mid", Num: 3700, Epic: cloudName, Parent: ciHealthEpic},
		boardItem{ID: "PVTI_leaf", Num: 3701, Parent: 3700},
	)
	r := runBoardSync(t, items, true, "")
	if want := []string{"PVTI_mid\t" + ciHealthID, "PVTI_leaf\t" + ciHealthID}; !slices.Equal(r.mutations, want) {
		t.Fatalf("mutations = %q, want %q\n%s", r.mutations, want, r)
	}
}

// A two-level unset chain converges in one --apply: both items are written,
// the leaf is not reported NO EPIC, and the board as written is consistent.
func TestBoardEpicsSyncConvergesATwoLevelUnsetChainInOnePass(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_mid", Num: 3600, Parent: cloudEpic},
		boardItem{ID: "PVTI_leaf", Num: 3601, Parent: 3600},
	)
	r := runBoardSync(t, items, true, "")
	if want := []string{"PVTI_mid\t" + cloudID, "PVTI_leaf\t" + cloudID}; !slices.Equal(r.mutations, want) {
		t.Fatalf("mutations = %q, want %q\n%s", r.mutations, want, r)
	}
	if strings.Contains(r.stdout, "NO EPIC") {
		t.Fatalf("a leaf whose chain reaches an epic is not NO EPIC\n%s", r)
	}
	written := append(epicItems(),
		boardItem{ID: "PVTI_mid", Num: 3600, Epic: cloudName, Parent: cloudEpic},
		boardItem{ID: "PVTI_leaf", Num: 3601, Epic: cloudName, Parent: 3600},
	)
	second := runBoardSync(t, written, false, "")
	if second.code != 0 || !strings.Contains(second.stdout, "board is consistent") {
		t.Fatalf("the board as written must be consistent\n%s", second)
	}
}

// A mid-ticket whose parent is off the board stands on its own field: its
// unset child takes that value.
func TestBoardEpicsSyncFallsBackToAParentsFieldWhenTheChainLeavesTheBoard(t *testing.T) {
	items := append(epicItems(),
		boardItem{ID: "PVTI_mid", Num: 3900, Epic: cloudName, Parent: 9999},
		boardItem{ID: "PVTI_leaf", Num: 3901, Parent: 3900},
	)
	r := runBoardSync(t, items, true, "")
	if want := []string{"PVTI_leaf\t" + cloudID}; !slices.Equal(r.mutations, want) {
		t.Fatalf("mutations = %q, want %q\n%s", r.mutations, want, r)
	}
}
