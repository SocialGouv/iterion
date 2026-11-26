package bots

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The slot and order fields of a check run, in the shape the live rollup
// carries. Read on SocialGouv/iterion: PR #3803 (head 238088c55) holds two
// 🛡️ Trivy suites created in the same second (124125695425, its fs-scan
// cancelled as run 140112051708, and 124125697108, green as run
// 140112060286) and a govulncheck run from two apps (GitHub Actions 15368,
// code scanning 57789). Renovate PR #3791 (head 1382605fc) carries `prep`
// from three workflows of Actions (🏃 Runner Image, 🐳 Container Image,
// 🧱 Sandbox Base Images). Run 45860683307 on 0f8a86725 was rerun: attempt 2
// stays in suite 124077495176 and copies the jobs it does not rerun with
// new, larger ids and their first startedAt.
const (
	actionsApp      = 15368
	codeScanningApp = 57789
	socketApp       = 156372
)

// rollupOid is the commit every stubbed rollup page is about unless it says
// otherwise, and rollupPin the pin check_rollup is asked to read it on.
const rollupOid, rollupPin = "238088c55a219ccc949f06c51a37dd826e51f233", "238088c55"

// at places a check run: its suite's app, createdAt and id, the run's
// startedAt ("" for null) and id. check_rollup reads only the app and the run
// id; the fixtures carry the suite's createdAt and id and the run's
// startedAt as GitHub does, so an ordering by any of them would be seen.
type at struct {
	app          int64
	suiteCreated string
	suite        int64
	started      string
	run          int64
}

// liveCheckRun returns a copy of the CheckRun fixture n stamped with the
// slot and order fields of the live green fs-scan run of PR #3803.
func liveCheckRun(n map[string]any) map[string]any {
	return checkRunAt(n, at{actionsApp, "2026-11-26T18:47:02Z", 124125697108, "2026-11-26T18:47:05Z", 140112060286})
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
	out["checkSuite"] = map[string]any{"createdAt": p.suiteCreated, "databaseId": p.suite,
		"app": map[string]any{"databaseId": p.app}}
	return out
}

// rollupPage is one page of the stubbed rollup: its nodes, the commit it is
// about ("" = rollupOid), and whether the read answers errors instead, as
// GitHub answers FORBIDDEN on a field the token may not read. A page says a
// next one follows when the rollup holds one, or when more is set.
type rollupPage struct {
	Nodes     []map[string]any `json:"nodes"`
	Oid       string           `json:"oid,omitempty"`
	Forbidden bool             `json:"forbidden,omitempty"`
	More      bool             `json:"more,omitempty"`
}

// rollupPagesProg runs the check_rollup of a required-checks block — the
// code under test, not a re-implementation — once per rollup of the JSON
// file it is given, each read through a stub of the GraphQL read that serves
// page i at cursor "c<i>" and raises, as the bot's gql does, on an error
// answer or a cursor it never handed out. It prints each result as JSON
// ("null" when the rollup is unreadable, "raised …" when the read raised),
// then every query it was sent.
const rollupPagesProg = `
out, queries = [], set()
for pages in json.load(open(sys.argv[1])):
    def gql(q, v, pages=pages):
        queries.add(q)
        after = v.get("after")
        if after is None:
            i = 0
        elif isinstance(after, str) and after.startswith("c") and after[1:].isdigit() and 0 < int(after[1:]) < len(pages):
            i = int(after[1:])
        else:
            raise RuntimeError("the stub never handed out the cursor %r" % (after,))
        p = pages[i]
        if p.get("forbidden"):
            raise ValueError("the GraphQL read answered errors")
        more = i + 1 < len(pages) or bool(p.get("more"))
        info = {"hasNextPage": more, "endCursor": "c%d" % (i + 1) if more else None}
        return {"repository": {"pullRequest": {"commits": {"nodes": [{"commit": {"oid": p.get("oid") or OID,
            "statusCheckRollup": {"contexts": {"pageInfo": info, "nodes": p["nodes"]}}}}]}}}}
    try:
        out.append(json.dumps(check_rollup(gql, "SocialGouv", "iterion", 3803, PIN)))
    except Exception as e:
        out.append("raised " + type(e).__name__ + ": " + str(e))
print(json.dumps({"out": out, "queries": sorted(queries)}))
`

// rollupPageVerdicts runs node's check_rollup over each paged rollup and
// returns each result, and every query the reads sent.
func rollupPageVerdicts(t *testing.T, node string, rollups [][]rollupPage) ([]string, []string) {
	t.Helper()
	const open, closing = "# >>> vetty-required-checks/v1", "# <<< vetty-required-checks/v1"
	prog := "import json, sys\n" + sentinelBlock(t, node, open, closing) + "\n" +
		fmt.Sprintf("OID, PIN = %q, %q\n", rollupOid, rollupPin) + rollupPagesProg
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
	var got struct {
		Out     []string `json:"out"`
		Queries []string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("check_rollup of %s printed %q: %v", node, raw, err)
	}
	if len(got.Out) != len(rollups) {
		t.Fatalf("check_rollup of %s answered %d rollups of %d", node, len(got.Out), len(rollups))
	}
	return got.Out, got.Queries
}

// rollupVerdicts runs node's check_rollup once per one-page rollup and
// returns each result as JSON ("null" when the rollup is unreadable).
func rollupVerdicts(t *testing.T, node string, rollups [][]map[string]any) []string {
	t.Helper()
	paged := make([][]rollupPage, len(rollups))
	for i, nodes := range rollups {
		paged[i] = []rollupPage{{Nodes: nodes}}
	}
	out, _ := rollupPageVerdicts(t, node, paged)
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

// rollupNodes are the three copies of the required-checks block.
var rollupNodes = []string{"continuity", "post_feedback", "arm_automerge"}

// TestDepUpdateGuardRollupJudgesTheNewestRun: a sha carrying several runs of
// one check (a rerun, or a suite a later trigger cancelled) is judged, per
// app and name, by its newest run, the run GitHub holds a required check to,
// in every node order. Mutations: read every run (drop the newest
// selection); keep the oldest (smallest id); order by the suite first; order
// by startedAt; slot by name alone (drop the app).
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
		return at{actionsApp, aCreated, 124077495176, started, id}
	}
	inB := func(started string, id int64) at {
		return at{actionsApp, bCreated, 124088000000, started, id}
	}
	// PR #3803's two 🛡️ Trivy suites, created in the same second.
	const stamp = "2026-11-26T18:47:02Z"
	trivyOld := at{actionsApp, stamp, 124125695425, stamp, 140112051708}
	trivyNew := at{actionsApp, stamp, 124125697108, "2026-11-26T18:47:05Z", 140112060286}
	gate := map[string]any{"__typename": "StatusContext", "context": "vetty/deps", "state": "SUCCESS", "isRequired": true}
	cases := []struct {
		name  string
		nodes []map[string]any
		want  string
	}{
		{"PR #3803 live: a superseded cancelled suite under a newer green one reads as passing", []map[string]any{
			done("fs-scan", "CANCELLED", true, trivyOld),
			done("fs-scan", "SUCCESS", true, trivyNew),
			done("govulncheck", "SUCCESS", false, at{actionsApp, stamp, 124125697176, "2026-11-26T18:47:56Z", 140112173007}),
			done("govulncheck", "SUCCESS", false, at{codeScanningApp, "2026-11-26T18:47:34Z", 124125880038, "2026-11-26T18:50:57Z", 140113738592}),
			done("test", "SUCCESS", true, at{actionsApp, stamp, 124125697176, "2026-11-26T19:13:32Z", 140123285038}),
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
			done("govulncheck", "FAILURE", true, at{actionsApp, stamp, 124125697176, "2026-11-26T18:47:56Z", 140112173007}),
			done("govulncheck", "SUCCESS", false, at{codeScanningApp, "2026-11-26T18:47:34Z", 124125880038, "2026-11-26T18:50:57Z", 140113738592}),
		}, `[["govulncheck"], [], []]`},
		// Neither app is Actions: only the app tells the two runs apart.
		{"two apps' runs of one name: an older red from code scanning stays red behind a newer green from Socket", []map[string]any{
			done("Security scan", "FAILURE", true, at{codeScanningApp, "2026-11-26T19:24:38Z", 124138550001, "2026-11-26T19:24:39Z", 140128190001}),
			done("Security scan", "SUCCESS", true, at{socketApp, "2026-11-26T19:24:40Z", 124138553419, "2026-11-26T19:24:45Z", 140128194110}),
			gate,
		}, `[["Security scan"], [], ["Security scan", "vetty/deps"]]`},
		// One app's runs of one name share their slot whatever workflow they
		// come from: GitHub holds a required check to the newest run of its
		// name. The three prep runs are optional, so the reading changes only
		// the informational optional list: the newest (140128196517, green)
		// passes and the older red one is superseded.
		{"PR #3791 shape: one name from three workflows of one app is judged by its newest run", []map[string]any{
			done("prep", "SUCCESS", false, at{actionsApp, "2026-11-26T19:24:43Z", 124138581364, "2026-11-26T19:24:52Z", 140128196517}),
			done("prep", "FAILURE", false, at{actionsApp, "2026-11-26T19:24:43Z", 124138581415, "2026-11-26T19:24:47Z", 140128196075}),
			done("prep", "SUCCESS", false, at{actionsApp, "2026-11-26T19:24:43Z", 124138581550, "2026-11-26T19:24:52Z", 140128196296}),
			gate,
		}, `[[], [], ["vetty/deps"]]`},
	}
	for _, node := range rollupNodes {
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
// missing or malformed id, or a shared id) makes the rollup unreadable, as no
// run may win by its position. Mutations: require the fields on lone runs;
// drop any one of the slot checks.
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
		at{socketApp, "2026-11-26T19:24:40Z", 124138553419, "2026-11-26T19:24:45Z", 140128194110})
	for _, node := range rollupNodes {
		lone := map[string]map[string]any{
			"a run whose suite has no app":      edit(socket, func(_, s map[string]any) { s["app"] = nil }),
			"a run whose suite app has no id":   edit(socket, func(_, s map[string]any) { s["app"] = map[string]any{} }),
			"a run with no checkSuite":          edit(socket, func(n, _ map[string]any) { delete(n, "checkSuite") }),
			"a completed run with no startedAt": edit(socket, func(n, _ map[string]any) { n["startedAt"] = nil }),
			"a run with no databaseId":          edit(socket, func(n, _ map[string]any) { delete(n, "databaseId") }),
			"a run databaseId given as text":    edit(socket, func(n, _ map[string]any) { n["databaseId"] = "140128194110" }),
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
		older := checkRunAt(green, at{actionsApp, "2026-11-26T18:47:02Z", 124125695425, "2026-11-26T18:47:02Z", 140112051708})
		several := map[string][]map[string]any{
			"two runs of a slot, one with no databaseId": {older, edit(green, func(n, _ map[string]any) { delete(n, "databaseId") })},
			"two runs of a slot, one id given as text":   {older, edit(green, func(n, _ map[string]any) { n["databaseId"] = "140112060286" })},
			"two runs of a slot, one id given as a bool": {older, edit(green, func(n, _ map[string]any) { n["databaseId"] = true })},
			"two runs of a slot sharing one id":          {older, edit(green, func(n, _ map[string]any) { n["databaseId"] = int64(140112051708) })},
			"two runs of a name whose suites carry no app": {
				edit(older, func(_, s map[string]any) { s["app"] = nil }),
				edit(green, func(_, s map[string]any) { s["app"] = nil }),
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

// pr3791Rollup is the live rollup of renovate PR #3791 (85 contexts) and the
// run ids of its 🧪 Tests suite, from testdata.
func pr3791Rollup(t *testing.T) ([]map[string]any, []int64) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vetty_rollup_pr3791.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Oid        string           `json:"oid"`
		TestsSuite []int64          `json:"testsSuite"`
		Nodes      []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Nodes) != 85 || len(fx.TestsSuite) != 23 {
		t.Fatalf("the #3791 fixture holds %d contexts and %d Tests runs, want 85 and 23", len(fx.Nodes), len(fx.TestsSuite))
	}
	return fx.Nodes, fx.TestsSuite
}

// split cuts nodes into pages of the given sizes, which must add up.
func split(t *testing.T, nodes []map[string]any, sizes ...int) []rollupPage {
	t.Helper()
	var pages []rollupPage
	for _, n := range sizes {
		if n > len(nodes) {
			t.Fatalf("page sizes %v exceed %d nodes", sizes, len(nodes))
		}
		pages = append(pages, rollupPage{Nodes: nodes[:n]})
		nodes = nodes[n:]
	}
	if len(nodes) != 0 {
		t.Fatalf("page sizes %v leave %d nodes", sizes, len(nodes))
	}
	return pages
}

// TestDepUpdateGuardRollupReadsEveryPage: the rollup is read whole across
// its pages, each about the same commit, then deduplicated over the union.
// Renovate PR #3791 already carries 85 contexts; once a labelled run cancels
// the opened one (#3738), a second, cancelled copy of its 🧪 Tests suite
// takes it past one page of 100. Every split and many node orders of that
// rollup read as its live one does; a page about another commit, a page that
// names no next one, or more pages than the bound read as unreadable.
// Mutations: read the first page only; accept a page about another commit
// that shares the pin's prefix; take the smallest id.
func TestDepUpdateGuardRollupReadsEveryPage(t *testing.T) {
	live, tests := pr3791Rollup(t)
	const want = `[["revi/review"], [], ["brand", "golangci", "test", "vendor-check"]]`
	inTests := map[int64]bool{}
	for _, id := range tests {
		inTests[id] = true
	}
	// The opened run's suite, cancelled by the labelled one: created first,
	// so every run of it has a smaller id; a skipped job stays skipped.
	var cancelled []map[string]any
	for _, n := range live {
		id, ok := n["databaseId"].(float64)
		if !ok || !inTests[int64(id)] {
			continue
		}
		c := maps.Clone(n)
		c["databaseId"] = int64(id) - 50_000
		c["status"] = "COMPLETED"
		if c["conclusion"] != "SKIPPED" {
			c["conclusion"] = "CANCELLED"
		}
		cancelled = append(cancelled, c)
	}
	doubled := append(slices.Clone(live), cancelled...)
	if len(doubled) != 108 {
		t.Fatalf("the doubled rollup holds %d contexts, want 108", len(doubled))
	}
	// The orders: as served (the cancelled suite last, straddling the page
	// boundary of 100), reversed, the cancelled suite in the middle, and
	// seeded shuffles.
	orders := [][]map[string]any{doubled, slices.Clone(doubled)}
	slices.Reverse(orders[1])
	middle := append(append(slices.Clone(live[:50]), cancelled...), live[50:]...)
	orders = append(orders, middle)
	r := rand.New(rand.NewPCG(3806, 2))
	for range 20 {
		o := slices.Clone(doubled)
		r.Shuffle(len(o), func(i, j int) { o[i], o[j] = o[j], o[i] })
		orders = append(orders, o)
	}
	splits := [][]int{{100, 8}, {54, 54}, {40, 40, 28}, {1, 106, 1}}
	for _, node := range rollupNodes {
		if got := rollupVerdicts(t, node, [][]map[string]any{live}); got[0] != want {
			t.Fatalf("%s: the live #3791 rollup reads %s, want %s: the cases below would prove nothing", node, got[0], want)
		}
		var rollups [][]rollupPage
		var names []string
		for oi, o := range orders {
			for _, sz := range splits {
				rollups = append(rollups, split(t, o, sz...))
				names = append(names, fmt.Sprintf("order %d, pages %v", oi, sz))
			}
		}
		got, _ := rollupPageVerdicts(t, node, rollups)
		for i, g := range got {
			if g != want {
				t.Errorf("%s: #3791 with a cancelled Tests suite, %s: it reads %s, want %s", node, names[i], g, want)
			}
		}

		// A head that moves between two pages: the second page is about
		// another commit, one sharing the pin's prefix included.
		twoPages := split(t, doubled, 100, 8)
		moved := func(oid string) []rollupPage {
			p := slices.Clone(twoPages)
			p[1].Oid = oid
			return p
		}
		// The bound: 20 pages are read, a 21st is not.
		page := func(i int) []map[string]any {
			return []map[string]any{checkRunAt(map[string]any{"name": fmt.Sprintf("job %d", i), "status": "COMPLETED",
				"conclusion": "SUCCESS", "isRequired": true}, at{actionsApp, "2026-11-26T19:24:43Z", 124138581566, "", int64(140128200000 + i)})}
		}
		var twenty, endless []rollupPage
		var twentyNames []string
		for i := range 20 {
			twenty = append(twenty, rollupPage{Nodes: page(i)})
			twentyNames = append(twentyNames, fmt.Sprintf("job %d", i))
		}
		slices.Sort(twentyNames)
		endless = append(slices.Clone(twenty), rollupPage{Nodes: page(20)})
		named := slices.Clone(twenty)
		named[19].More = true
		passing, _ := json.Marshal([]any{[]string{}, []string{}, twentyNames})
		cases := []struct {
			name  string
			pages []rollupPage
			want  string
		}{
			{"the second page about another commit", moved("1382605fc789786b60918fb7a4964420aa1e60be"), "null"},
			{"the second page about another commit sharing the pin's prefix", moved(rollupOid[:12] + strings.Repeat("0", 28)), "null"},
			{"the first page about another commit", func() []rollupPage {
				p := slices.Clone(twoPages)
				p[0].Oid = "1382605fc789786b60918fb7a4964420aa1e60be"
				return p
			}(), "null"},
			{"twenty pages, the bound", twenty, strings.ReplaceAll(string(passing), ",", ", ")},
			{"twenty-one pages, past the bound", endless, "null"},
			{"a twentieth page saying another follows", named, "null"},
		}
		for _, tc := range cases {
			got, _ := rollupPageVerdicts(t, node, [][]rollupPage{tc.pages})
			if got[0] != tc.want {
				t.Errorf("%s: %s: it reads %s, want %s", node, tc.name, got[0], tc.want)
			}
		}
	}
}

// TestDepUpdateGuardRollupAsksOnlyWhatTheRunTokenReads: the rollup query
// selects only fields the run token can read — Checks, Commit statuses,
// Pull requests (read) — so it never asks for checkSuite.workflowRun, which
// needs Actions (read): the run token never carries it, and on a private
// repository GitHub refuses the whole read. A read that answers errors
// (FORBIDDEN on any path) raises from check_rollup, to the caller's explicit
// refusal, on any page. Mutations: query workflowRun again; swallow the
// error into a reading.
func TestDepUpdateGuardRollupAsksOnlyWhatTheRunTokenReads(t *testing.T) {
	// Every name the query may carry, each read under a permission the run
	// token holds (RunCIReadPermissions and the baseline, see
	// pkg/forge/github/app_client.go): the operation and its variables, the
	// pull request and its last commit (Pull requests), the rollup and its
	// pages, StatusContext (Commit statuses), CheckRun, its suite and the
	// suite's app (Checks: the REST check-runs payload carries the run id and
	// app id under Checks read).
	allowed := map[string]bool{}
	for _, w := range strings.Fields("query owner name number after String Int repository pullRequest commits last nodes " +
		"commit oid statusCheckRollup contexts first pageInfo hasNextPage endCursor __typename on " +
		"StatusContext context state isRequired pullRequestNumber " +
		"CheckRun status conclusion databaseId checkSuite app") {
		allowed[w] = true
	}
	green := liveCheckRun(map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS", "isRequired": true})
	const raised = "raised ValueError: the GraphQL read answered errors"
	word := regexp.MustCompile(`[A-Za-z_]+`)
	for _, node := range rollupNodes {
		got, queries := rollupPageVerdicts(t, node, [][]rollupPage{
			{{Nodes: []map[string]any{green}}},
			{{Nodes: []map[string]any{green}, Forbidden: true}},
			{{Nodes: []map[string]any{green}}, {Nodes: []map[string]any{green}, Forbidden: true}},
		})
		if len(queries) != 1 {
			t.Fatalf("%s: check_rollup sent %d distinct queries, want 1: %q", node, len(queries), queries)
		}
		for _, w := range word.FindAllString(queries[0], -1) {
			if !allowed[w] {
				t.Errorf("%s: the rollup query asks %q, which no permission of the run token is known to read:\n%s", node, w, queries[0])
			}
		}
		if got[0] != `[[], [], ["test"]]` {
			t.Errorf("%s: the readable rollup reads %s: the cases below would prove nothing", node, got[0])
		}
		for i, name := range []string{"a read answering FORBIDDEN", "a second page answering FORBIDDEN"} {
			if got[i+1] != raised {
				t.Errorf("%s: %s reads %s, want check_rollup to raise %q", node, name, got[i+1], raised)
			}
		}
	}
}
