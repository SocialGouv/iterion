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

// The slot and order fields of a check run, in the shape the live rollup
// carries. Read on SocialGouv/iterion: PR #3803 (head 238088c55) holds two
// 🛡️ Trivy suites created in the same second (124125695425, its fs-scan
// cancelled as run 140112051708, and 124125697108, green as run
// 140112060286) and a govulncheck run from two apps (GitHub Actions 15368,
// code scanning 57789, whose suite has no workflow run). Renovate PR #3791
// (head 1382605fc) carries `prep` from three workflows of Actions. Run
// 45860683307 on 0f8a86725 was rerun: attempt 2 stays in suite
// 124077495176 and copies the jobs it does not rerun with new, larger ids
// and their first startedAt.
const (
	actionsApp      = 15368
	codeScanningApp = 57789
	socketApp       = 156372

	testsWorkflow          = 248906335
	trivyWorkflow          = 272547055
	runnerImageWorkflow    = 319177077
	containerImageWorkflow = 271961636
	sandboxImageWorkflow   = 318005199
)

// at places a check run: its suite's app, workflow (0 for a suite with no
// workflow run), createdAt and id, the run's startedAt ("" for null) and id.
// The suite's createdAt and id are not read by check_rollup; the fixtures
// carry them as GitHub does, so an ordering by the suite would be seen.
type at struct {
	app, workflow int64
	suiteCreated  string
	suite         int64
	started       string
	run           int64
}

// liveCheckRun returns a copy of the CheckRun fixture n stamped with the
// slot and order fields of the live green fs-scan run of PR #3803.
func liveCheckRun(n map[string]any) map[string]any {
	return checkRunAt(n, at{actionsApp, trivyWorkflow, "2026-11-26T18:47:02Z", 124125697108, "2026-11-26T18:47:05Z", 140112060286})
}

// checkRunAt returns a copy of n placed at p.
func checkRunAt(n map[string]any, p at) map[string]any {
	out := maps.Clone(n)
	out["__typename"] = "CheckRun"
	out["databaseId"] = p.run
	out["startedAt"] = nil
	if p.started != "" {
		out["startedAt"] = p.started
	}
	var wrun any
	if p.workflow != 0 {
		wrun = map[string]any{"workflow": map[string]any{"databaseId": p.workflow}}
	}
	out["checkSuite"] = map[string]any{"createdAt": p.suiteCreated, "databaseId": p.suite,
		"app": map[string]any{"databaseId": p.app}, "workflowRun": wrun}
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
// app, workflow and name, by its newest run, the run GitHub holds a
// required check to, in every node order. Mutations: read every run (drop
// the newest selection); keep the oldest (smallest id); order by the suite
// first; order by startedAt; slot by name alone; slot by (app, name)
// without the workflow.
func TestDepUpdateGuardRollupJudgesTheNewestRun(t *testing.T) {
	run := func(name, status, conclusion string, required bool) map[string]any {
		var c any
		if conclusion != "" {
			c = conclusion
		}
		return map[string]any{"name": name, "status": status, "conclusion": c, "isRequired": required}
	}
	done := func(name, conclusion string, required bool, p at) map[string]any {
		return checkRunAt(run(name, "COMPLETED", conclusion, required), p)
	}
	// Run 45860683307 of 🧪 Tests on 0f8a86725 (suite A, created first) and
	// a later trigger of the same workflow on that sha (suite B, created
	// between A's first attempt and its rerun).
	const aCreated, bCreated = "2026-11-26T16:38:50Z", "2026-11-26T17:00:58Z"
	inA := func(started string, id int64) at {
		return at{actionsApp, testsWorkflow, aCreated, 124077495176, started, id}
	}
	inB := func(started string, id int64) at {
		return at{actionsApp, testsWorkflow, bCreated, 124088000000, started, id}
	}
	// PR #3803's two 🛡️ Trivy suites, created in the same second.
	const stamp = "2026-11-26T18:47:02Z"
	trivyOld := at{actionsApp, trivyWorkflow, stamp, 124125695425, stamp, 140112051708}
	trivyNew := at{actionsApp, trivyWorkflow, stamp, 124125697108, "2026-11-26T18:47:05Z", 140112060286}
	gate := map[string]any{"__typename": "StatusContext", "context": "vetty/deps", "state": "SUCCESS", "isRequired": true}
	cases := []struct {
		name  string
		nodes []map[string]any
		want  string
	}{
		{"PR #3803 live: a superseded cancelled suite under a newer green one reads as passing", []map[string]any{
			done("fs-scan", "CANCELLED", true, trivyOld),
			done("fs-scan", "SUCCESS", true, trivyNew),
			done("govulncheck", "SUCCESS", false, at{actionsApp, testsWorkflow, stamp, 124125697176, "2026-11-26T18:47:56Z", 140112173007}),
			done("govulncheck", "SUCCESS", false, at{codeScanningApp, 0, "2026-11-26T18:47:34Z", 124125880038, "2026-11-26T18:50:57Z", 140113738592}),
			done("test", "SUCCESS", true, at{actionsApp, testsWorkflow, stamp, 124125697176, "2026-11-26T19:13:32Z", 140123285038}),
			gate,
		}, `[[], [], ["fs-scan", "test", "vetty/deps"]]`},
		{"a newer failure after an older success reads as failing", []map[string]any{
			done("fs-scan", "SUCCESS", true, trivyOld), done("fs-scan", "FAILURE", true, trivyNew), gate,
		}, `[["fs-scan"], [], ["vetty/deps"]]`},
		{"an older suite's later rerun that fails beats a newer suite's earlier green run", []map[string]any{
			done("test", "FAILURE", true, inA("2026-11-26T17:05:42Z", 140064262870)),
			done("test", "SUCCESS", true, inB("2026-11-26T17:01:00Z", 140060000000)),
			done("test", "FAILURE", true, inA("2026-11-26T17:16:15Z", 140069806812)),
		}, `[["test"], [], []]`},
		{"an older suite's later green rerun beats a newer suite's cancelled run", []map[string]any{
			done("test", "SUCCESS", true, inA("2026-11-26T17:16:15Z", 140069806812)),
			done("test", "CANCELLED", true, inB("2026-11-26T17:01:00Z", 140060000000)),
		}, `[[], [], ["test"]]`},
		{"a rerun's copy of a green job keeps its first startedAt and still is the newest", []map[string]any{
			done("test-unit (0)", "SUCCESS", true, inA("2026-11-26T16:39:10Z", 140050588254)),
			done("test-unit (0)", "CANCELLED", true, inB("2026-11-26T17:01:00Z", 140060000001)),
			done("test-unit (0)", "SUCCESS", true, inA("2026-11-26T16:39:10Z", 140066285343)),
		}, `[[], [], ["test-unit (0)"]]`},
		{"a queued rerun with no startedAt is the newest run and pending", []map[string]any{
			done("test", "SUCCESS", true, inB("2026-11-26T17:01:00Z", 140060000000)),
			checkRunAt(run("test", "QUEUED", "", true), inA("", 140069806812)),
		}, `[["test"], [], []]`},
		{"a newer green run of the name from another app hides no red run of this app", []map[string]any{
			done("govulncheck", "FAILURE", true, at{actionsApp, testsWorkflow, stamp, 124125697176, "2026-11-26T18:47:56Z", 140112173007}),
			done("govulncheck", "SUCCESS", false, at{codeScanningApp, 0, "2026-11-26T18:47:34Z", 124125880038, "2026-11-26T18:50:57Z", 140113738592}),
		}, `[["govulncheck"], [], []]`},
		{"PR #3791 shape: one name from three workflows is three checks, and a red one stays red", []map[string]any{
			done("prep", "SUCCESS", false, at{actionsApp, runnerImageWorkflow, "2026-11-26T19:24:43Z", 124138581364, "2026-11-26T19:24:52Z", 140128196517}),
			done("prep", "FAILURE", false, at{actionsApp, containerImageWorkflow, "2026-11-26T19:24:43Z", 124138581415, "2026-11-26T19:24:47Z", 140128196075}),
			done("prep", "SUCCESS", false, at{actionsApp, sandboxImageWorkflow, "2026-11-26T19:24:43Z", 124138581550, "2026-11-26T19:24:52Z", 140128196296}),
			gate,
		}, `[[], ["prep"], ["vetty/deps"]]`},
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

// TestDepUpdateGuardRollupOrdersOnlySlotsOfSeveralRuns: a run alone in its
// slot needs no order, so a run with a null app, suite, startedAt or id
// (which the schema allows, say for a third-party app) is read on its
// conclusion; a slot of several runs where one cannot be placed (no app, a
// malformed workflow, a missing or malformed id, or a shared id) makes the
// rollup unreadable, as no run may win by its position. Mutations: require
// the fields on lone runs; drop any one of the slot checks.
func TestDepUpdateGuardRollupOrdersOnlySlotsOfSeveralRuns(t *testing.T) {
	green := liveCheckRun(map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS", "isRequired": true})
	if got := rollupVerdicts(t, "continuity", [][]map[string]any{{green}}); !reflect.DeepEqual(got, []string{`[[], [], ["test"]]`}) {
		t.Fatalf("the well-formed run reads %v, want it passing: the cases below would prove nothing", got)
	}
	edit := func(n map[string]any, f func(n, s map[string]any)) map[string]any {
		out := maps.Clone(n)
		s := maps.Clone(out["checkSuite"].(map[string]any))
		out["checkSuite"] = s
		f(out, s)
		return out
	}
	socket := checkRunAt(map[string]any{"name": "Socket Security: Project Report", "status": "COMPLETED", "conclusion": "SUCCESS", "isRequired": false},
		at{socketApp, 0, "2026-11-26T19:24:40Z", 124138553419, "2026-11-26T19:24:45Z", 140128194110})
	for _, node := range []string{"continuity", "post_feedback", "arm_automerge"} {
		lone := map[string]map[string]any{
			"a run whose suite has no app":       edit(socket, func(_, s map[string]any) { s["app"] = nil }),
			"a run whose suite app has no id":    edit(socket, func(_, s map[string]any) { s["app"] = map[string]any{} }),
			"a run with no checkSuite":           edit(socket, func(n, _ map[string]any) { delete(n, "checkSuite") }),
			"a completed run with no startedAt":  edit(socket, func(n, _ map[string]any) { n["startedAt"] = nil }),
			"a run with no databaseId":           edit(socket, func(n, _ map[string]any) { delete(n, "databaseId") }),
			"a run databaseId given as text":     edit(socket, func(n, _ map[string]any) { n["databaseId"] = "140128194110" }),
			"a workflow run with no workflow id": edit(socket, func(_, s map[string]any) { s["workflowRun"] = map[string]any{"workflow": nil} }),
		}
		for name, run := range lone {
			got := rollupVerdicts(t, node, permutations([]map[string]any{run, green}))
			for i, g := range got {
				if g != `[[], [], ["test"]]` {
					t.Errorf("%s: %s, alone in its slot (rollup %d): it reads %s, want it readable and test passing", node, name, i, g)
				}
			}
		}
		// A newer run of green's own slot, and an older one it can be
		// ordered against.
		older := checkRunAt(green, at{actionsApp, trivyWorkflow, "2026-11-26T18:47:02Z", 124125695425, "2026-11-26T18:47:02Z", 140112051708})
		several := map[string][]map[string]any{
			"two runs of a slot, one with no databaseId": {older, edit(green, func(n, _ map[string]any) { delete(n, "databaseId") })},
			"two runs of a slot, one id given as text":   {older, edit(green, func(n, _ map[string]any) { n["databaseId"] = "140112060286" })},
			"two runs of a slot, one id given as a bool": {older, edit(green, func(n, _ map[string]any) { n["databaseId"] = true })},
			"two runs of a slot sharing one id":          {older, edit(green, func(n, _ map[string]any) { n["databaseId"] = int64(140112051708) })},
			"two runs of a name whose suites carry no app": {
				edit(older, func(_, s map[string]any) { s["app"] = nil; s["workflowRun"] = nil }),
				edit(green, func(_, s map[string]any) { s["app"] = nil; s["workflowRun"] = nil }),
			},
			"two runs of a name, one from a workflow run with no workflow id": {
				edit(older, func(_, s map[string]any) { s["workflowRun"] = nil }),
				edit(green, func(_, s map[string]any) { s["workflowRun"] = map[string]any{"workflow": map[string]any{}} }),
			},
		}
		for name, runs := range several {
			got := rollupVerdicts(t, node, permutations(runs))
			for i, g := range got {
				if g != "null" {
					t.Errorf("%s: %s (rollup %d): it reads %s, want the rollup unreadable (null)", node, name, i, g)
				}
			}
		}
	}
}
