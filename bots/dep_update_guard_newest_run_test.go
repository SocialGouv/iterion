package bots

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The ordering fields of a check run, in the shape the live rollup carries:
// read on SocialGouv/iterion PR #3803 (head 238088c55), whose sha holds two
// 🧪 Tests suites created in the same second (124125695425 and
// 124125697108, both 2026-11-26T18:47:02Z) and a govulncheck run from two
// apps (GitHub Actions 15368, code scanning 57789).
const (
	actionsApp      = 15368
	codeScanningApp = 57789
)

// liveCheckRun returns a copy of the CheckRun fixture n stamped with the
// ordering fields of the live green fs-scan run of PR #3803: every reader
// of the rollup needs them, as a run without them makes it unreadable.
func liveCheckRun(n map[string]any) map[string]any {
	return checkRunAt(n, actionsApp, "2026-11-26T18:47:02Z", 124125697108, "2026-11-26T18:47:05Z", 140112060286)
}

// checkRunAt returns a copy of n carrying a suite (app, createdAt, id) and
// the run's own startedAt and id; an empty started leaves startedAt null.
func checkRunAt(n map[string]any, app int64, suiteCreated string, suiteID int64, started string, runID int64) map[string]any {
	out := maps.Clone(n)
	out["__typename"] = "CheckRun"
	out["databaseId"] = runID
	out["startedAt"] = nil
	if started != "" {
		out["startedAt"] = started
	}
	out["checkSuite"] = map[string]any{"createdAt": suiteCreated, "databaseId": suiteID,
		"app": map[string]any{"databaseId": app}}
	return out
}

// rollupVerdicts runs the check_rollup of node's required-checks block —
// the code under test, not a re-implementation — once per rollup, each
// answered by a stub of the GraphQL read, and returns each result as JSON
// ("null" when the rollup is unreadable).
func rollupVerdicts(t *testing.T, node string, rollups [][]map[string]any) []string {
	t.Helper()
	const open, closing = "# >>> vetty-required-checks/v1", "# <<< vetty-required-checks/v1"
	block := sentinelBlock(t, node, open, closing)
	prog := "import json, sys\n" +
		block + "\n" +
		"out = []\n" +
		"for nodes in json.load(open(sys.argv[1])):\n" +
		"    data = {'repository': {'pullRequest': {'commits': {'nodes': [{'commit': {'oid': '238088c55a219ccc949f06c51a37dd826e51f233',\n" +
		"        'statusCheckRollup': {'contexts': {'pageInfo': {'hasNextPage': False}, 'nodes': nodes}}}}]}}}}\n" +
		"    out.append(json.dumps(check_rollup(lambda q, v: data, 'SocialGouv', 'iterion', 3803, '238088c55')))\n" +
		"print(json.dumps(out))\n"
	dir := t.TempDir()
	progPath, inPath := filepath.Join(dir, "rollup.py"), filepath.Join(dir, "rollups.json")
	in, err := json.Marshal(rollups)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(progPath, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inPath, in, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("python3", "-I", progPath, inPath).CombinedOutput()
	if err != nil {
		t.Fatalf("check_rollup of %s: %v\n%s", node, err, raw)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("check_rollup of %s printed %q: %v", node, raw, err)
	}
	return out
}

// permutations returns every order of nodes: a verdict that one node order
// alone yields is decided by the order, which nothing guarantees.
func permutations(nodes []map[string]any) [][]map[string]any {
	if len(nodes) <= 1 {
		return [][]map[string]any{append([]map[string]any(nil), nodes...)}
	}
	var out [][]map[string]any
	for i := range nodes {
		rest := append(append([]map[string]any(nil), nodes[:i]...), nodes[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]map[string]any{nodes[i]}, p...))
		}
	}
	return out
}

// TestDepUpdateGuardRollupJudgesTheNewestRun: a sha carrying several runs of
// one check (a rerun, or a suite a later trigger cancelled) is judged, per
// app and name, by its newest run — the run GitHub holds a required check
// to — in every node order. Mutations: read every run (drop the newest
// selection); keep the oldest; key the selection by name alone.
func TestDepUpdateGuardRollupJudgesTheNewestRun(t *testing.T) {
	const stamp = "2026-11-26T18:47:02Z"
	run := func(name, status, conclusion string, required bool) map[string]any {
		var c any
		if conclusion != "" {
			c = conclusion
		}
		return map[string]any{"name": name, "status": status, "conclusion": c, "isRequired": required}
	}
	// The cancelled suite and the green one are created in the same second,
	// as on PR #3803: the suite's databaseId breaks the tie.
	superseded := func(name, conclusion string, runID int64) map[string]any {
		return checkRunAt(run(name, "COMPLETED", conclusion, true), actionsApp, stamp, 124125695425, stamp, runID)
	}
	current := func(name, conclusion string, runID int64) map[string]any {
		return checkRunAt(run(name, "COMPLETED", conclusion, true), actionsApp, stamp, 124125697108, "2026-11-26T18:47:05Z", runID)
	}
	gate := map[string]any{"__typename": "StatusContext", "context": "vetty/deps", "state": "SUCCESS", "isRequired": true}
	cases := []struct {
		name  string
		nodes []map[string]any
		want  string
	}{
		{"a superseded cancelled suite under a newer green one reads as passing", []map[string]any{
			superseded("test", "CANCELLED", 140112051708), superseded("brand", "CANCELLED", 140112051709),
			current("test", "SUCCESS", 140112060286), current("brand", "SUCCESS", 140112060287), gate,
		}, `[[], [], ["brand", "test", "vetty/deps"]]`},
		{"a newer failure after an older success reads as failing", []map[string]any{
			superseded("test", "SUCCESS", 140112051708), current("test", "FAILURE", 140112060286), gate,
		}, `[["test"], [], ["vetty/deps"]]`},
		{"an older suite created earlier loses to a newer one whatever its ids", []map[string]any{
			checkRunAt(run("test", "COMPLETED", "FAILURE", true), actionsApp, "2026-11-26T18:47:01Z", 124125699999, "2026-11-26T18:47:09Z", 140112099999),
			current("test", "SUCCESS", 140112060286),
		}, `[[], [], ["test"]]`},
		{"a rerun inside one suite: the later start wins", []map[string]any{
			checkRunAt(run("test", "COMPLETED", "FAILURE", true), actionsApp, stamp, 124125697108, "2026-11-26T18:47:05Z", 140112060286),
			checkRunAt(run("test", "COMPLETED", "SUCCESS", true), actionsApp, stamp, 124125697108, "2026-11-26T19:13:32Z", 140112060285),
		}, `[[], [], ["test"]]`},
		{"a rerun started in the same second: the run's databaseId breaks the tie", []map[string]any{
			checkRunAt(run("test", "COMPLETED", "SUCCESS", true), actionsApp, stamp, 124125697108, "2026-11-26T18:47:05Z", 140112060286),
			checkRunAt(run("test", "COMPLETED", "FAILURE", true), actionsApp, stamp, 124125697108, "2026-11-26T18:47:05Z", 140112060287),
		}, `[["test"], [], []]`},
		{"a queued rerun with no startedAt is the newest of its suite and pending", []map[string]any{
			checkRunAt(run("test", "COMPLETED", "SUCCESS", true), actionsApp, stamp, 124125697108, "2026-11-26T18:47:05Z", 140112060287),
			checkRunAt(run("test", "QUEUED", "", true), actionsApp, stamp, 124125697108, "", 140112060286),
		}, `[["test"], [], []]`},
		{"a newer green run of the name from another app hides no red run of this app", []map[string]any{
			checkRunAt(run("govulncheck", "COMPLETED", "FAILURE", true), actionsApp, stamp, 124125697176, "2026-11-26T18:47:56Z", 140112173007),
			checkRunAt(run("govulncheck", "COMPLETED", "SUCCESS", false), codeScanningApp, "2026-11-26T18:47:34Z", 124125880038, "2026-11-26T18:50:57Z", 140113738592),
		}, `[["govulncheck"], [], []]`},
	}
	for _, node := range []string{"continuity", "post_feedback", "arm_automerge"} {
		for _, tc := range cases {
			perms := permutations(tc.nodes)
			got := rollupVerdicts(t, node, perms)
			wrong := -1
			n := 0
			for i, g := range got {
				if g != tc.want {
					if wrong < 0 {
						wrong = i
					}
					n++
				}
			}
			if wrong >= 0 {
				t.Errorf("%s: %s: %d of %d node orders misread, first %s: it reads %s, want %s",
					node, tc.name, n, len(perms), rollupOrder(perms[wrong]), got[wrong], tc.want)
			}
		}
	}
}

// rollupOrder names each node of a rollup with its outcome and run id.
func rollupOrder(nodes []map[string]any) string {
	var parts []string
	for _, n := range nodes {
		name, _ := n["name"].(string)
		if name == "" {
			name, _ = n["context"].(string)
		}
		parts = append(parts, fmt.Sprintf("%s:%v/%v#%v", name, n["status"], n["conclusion"], n["databaseId"]))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestDepUpdateGuardRollupNeedsEachRunsOrder: a check run whose ordering
// fields are missing or malformed makes the rollup unreadable — a run that
// cannot be placed must not win by its position. The one absence GitHub
// documents, no startedAt on a run that has not started, is read as
// pending (TestDepUpdateGuardRollupJudgesTheNewestRun). Mutation: drop any
// one of the field checks.
func TestDepUpdateGuardRollupNeedsEachRunsOrder(t *testing.T) {
	green := liveCheckRun(map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS", "isRequired": true})
	if got := rollupVerdicts(t, "continuity", [][]map[string]any{{green}}); !reflect.DeepEqual(got, []string{`[[], [], ["test"]]`}) {
		t.Fatalf("the well-formed run reads %v, want it passing: the cases below would prove nothing", got)
	}
	suite := func(edit func(s map[string]any)) map[string]any {
		n := maps.Clone(green)
		s := maps.Clone(n["checkSuite"].(map[string]any))
		edit(s)
		n["checkSuite"] = s
		return n
	}
	run := func(edit func(n map[string]any)) map[string]any {
		n := maps.Clone(green)
		edit(n)
		return n
	}
	cases := map[string]map[string]any{
		"a completed run with no startedAt": run(func(n map[string]any) { n["startedAt"] = nil }),
		"a run with no startedAt key":       run(func(n map[string]any) { delete(n, "startedAt") }),
		"a malformed startedAt":             run(func(n map[string]any) { n["startedAt"] = "2026-11-26 18:47:05" }),
		"a run with no databaseId":          run(func(n map[string]any) { delete(n, "databaseId") }),
		"a run databaseId given as text":    run(func(n map[string]any) { n["databaseId"] = "140112060286" }),
		"a run databaseId given as a bool":  run(func(n map[string]any) { n["databaseId"] = true }),
		"a run with no checkSuite":          run(func(n map[string]any) { delete(n, "checkSuite") }),
		"a suite with no createdAt":         suite(func(s map[string]any) { delete(s, "createdAt") }),
		"a malformed suite createdAt":       suite(func(s map[string]any) { s["createdAt"] = "yesterday" }),
		"a suite with no databaseId":        suite(func(s map[string]any) { s["databaseId"] = nil }),
		"a suite with no app":               suite(func(s map[string]any) { s["app"] = nil }),
		"a suite app with no databaseId":    suite(func(s map[string]any) { s["app"] = map[string]any{} }),
	}
	for name, bad := range cases {
		// Alone, and next to a well-formed newer run of its name: either
		// way the rollup cannot be read whole.
		newer := checkRunAt(green, actionsApp, "2026-11-26T18:50:00Z", 124125699999, "2026-11-26T18:50:01Z", 140112099999)
		got := rollupVerdicts(t, "continuity", append(permutations([]map[string]any{bad, newer}), []map[string]any{bad}))
		for i, g := range got {
			if g != "null" {
				t.Errorf("%s (rollup %d): it reads %s, want the rollup unreadable (null)", name, i, g)
			}
		}
	}
}
