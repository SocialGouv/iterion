package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// scriptWalls reads the longest time a campaign tool node's commands may take,
// at their walls, off its REAL script: every subprocess it runs, on the
// longest path through it. A command must carry a literal wall — `timeout=`
// on subprocess.run, or the `timeout` of the script's own `git` wrapper —
// and none may run in a loop, whose count nothing bounds; either is refused
// as unbounded. The steward's acts are its own: each starts only where it
// ends before the steward's reserve, so its walls are read apart.
type scriptWalls struct {
	// Outside is the longest path through the script with each act left out.
	Outside int `json:"outside"`
	// Tail is the longest path through one act after its record and its
	// counter-test (the `sh` calls the act's start counts twice its lot's wall
	// for), and Records the number of those `sh` calls.
	Tail    int `json:"tail"`
	Records int `json:"records"`
	// After is the longest path through what follows the steward's act loop:
	// the journal commit.
	After int `json:"after"`
}

// wallsAnalyzer is the reading scriptWalls describes, over Python's own
// parse of the script — template references are replaced by None first,
// since the script is read, not run.
const wallsAnalyzer = `
import ast, json, re, sys

src = re.sub(r"\{\{[^}]*\}\}", "None", open(sys.argv[1]).read())
mode = sys.argv[2]
tree = ast.parse(src)
defs = {n.name: n for n in ast.walk(tree) if isinstance(n, ast.FunctionDef)}

class Unbounded(Exception):
    pass

def lit(node):
    if isinstance(node, ast.Constant) and type(node.value) is int:
        return node.value
    return None

def kw(call, name):
    for k in call.keywords:
        if k.arg == name:
            return k.value
    return None

def git_default():
    fn = defs.get("git")
    if fn is None:
        raise Unbounded("the script runs git() and defines no git wrapper")
    for a, d in zip(fn.args.kwonlyargs, fn.args.kw_defaults):
        if a.arg == "timeout" and lit(d) is not None:
            return lit(d)
    raise Unbounded("the git wrapper has no literal timeout default")

ctx = {"records": None}
memo, active = {}, set()

def call_wall(c):
    f = c.func
    if isinstance(f, ast.Attribute) and isinstance(f.value, ast.Name) and f.value.id in ("subprocess", "os"):
        if f.value.id == "os" and f.attr not in ("system", "popen", "spawnl", "spawnv", "execv", "execvp"):
            return 0
        if f.value.id == "subprocess" and f.attr == "run" and lit(kw(c, "timeout")) is not None:
            return lit(kw(c, "timeout"))
        raise Unbounded("%s.%s with no literal timeout at line %d" % (f.value.id, f.attr, c.lineno))
    if isinstance(f, ast.Name):
        if f.id == "git":
            t = kw(c, "timeout")
            if t is None:
                return git_default()
            if lit(t) is None:
                raise Unbounded("git() with a timeout that is not a literal at line %d" % c.lineno)
            return lit(t)
        if f.id == "sh":
            if ctx["records"] is None:
                raise Unbounded("sh() outside an act at line %d" % c.lineno)
            ctx["records"] += 1
            return 0
        if f.id in defs:
            return fn_wall(f.id)
    return 0

def expr_wall(node):
    if node is None:
        return 0
    total = 0
    stack = [node]
    while stack:
        n = stack.pop()
        if isinstance(n, (ast.Lambda, ast.FunctionDef)):
            continue
        if isinstance(n, ast.Call):
            total += call_wall(n)
        stack.extend(ast.iter_child_nodes(n))
    return total

def fn_wall(name):
    if name in memo:
        return memo[name]
    if name in active:
        raise Unbounded("%s() calls itself" % name)
    active.add(name)
    fall, ex = seq(defs[name].body)
    active.discard(name)
    memo[name] = mx(fall, ex) or 0
    return memo[name]

def mx(*xs):
    xs = [x for x in xs if x is not None]
    return max(xs) if xs else None

def add(a, b):
    return None if b is None else a + b

def seq(stmts, skip=None):
    total, ex = 0, None
    for st in stmts:
        if skip is not None and st is skip:
            continue
        f, x = stmt(st, skip)
        ex = mx(ex, add(total, x))
        if f is None:
            return None, ex
        total += f
    return total, ex

def stmt(st, skip):
    if isinstance(st, (ast.FunctionDef, ast.ClassDef, ast.Import, ast.ImportFrom, ast.Pass, ast.Global, ast.Nonlocal)):
        return 0, None
    if isinstance(st, (ast.Continue, ast.Break)):
        return None, 0
    if isinstance(st, ast.Return):
        return None, expr_wall(st.value)
    if isinstance(st, ast.Raise):
        return None, expr_wall(st.exc)
    if isinstance(st, ast.If):
        t = expr_wall(st.test)
        bf, bx = seq(st.body, skip)
        of, ox = seq(st.orelse, skip)
        fall = mx(bf, of)
        return add(t, fall), add(t, mx(bx, ox))
    if isinstance(st, (ast.For, ast.While)):
        head = expr_wall(st.iter if isinstance(st, ast.For) else st.test)
        if st is ctx.get("loop"):
            return head, None
        bf, bx = seq(st.body, skip)
        if (mx(bf, bx) or 0) > 0:
            raise Unbounded("a command runs in a loop at line %d" % st.lineno)
        of, ox = seq(st.orelse, skip)
        return add(head, of), add(head, ox)
    if isinstance(st, ast.With):
        w = sum(expr_wall(i.context_expr) for i in st.items)
        bf, bx = seq(st.body, skip)
        return add(w, bf), add(w, bx)
    if isinstance(st, ast.Try):
        body = mx(*seq(st.body, skip)) or 0
        handlers = [mx(*seq(h.body, skip)) or 0 for h in st.handlers] + [mx(*seq(st.orelse, skip)) or 0]
        fin = mx(*seq(st.finalbody, skip)) or 0
        total = body + max(handlers) + fin
        return total, total
    return expr_wall(st), None

out = {"outside": 0, "tail": 0, "records": 0, "after": 0}
if mode == "steward":
    loop, check = None, None
    for n in ast.walk(tree):
        if isinstance(n, ast.For):
            for i, st in enumerate(n.body):
                if isinstance(st, ast.If) and any(isinstance(x, ast.Name) and x.id == "handoff_by" for x in ast.walk(st.test)):
                    loop, check = n, i
    if loop is None:
        print(json.dumps({"error": "no act loop: no loop whose body checks handoff_by"}))
        sys.exit(0)
    ctx["loop"] = loop
    try:
        pre_f, pre_x = seq(loop.body[:check])
        if (mx(pre_f, pre_x) or 0) > 0:
            raise Unbounded("a command runs for every pending request, before the act's own check")
        ctx["records"] = 0
        tf, tx = seq(loop.body[check + 1:])
        out["tail"], out["records"] = mx(tf, tx) or 0, ctx["records"]
        ctx["records"] = None
        top = next(i for i, st in enumerate(tree.body) if any(n is loop for n in ast.walk(st)))
        af, ax = seq(tree.body[top + 1:])
        out["after"] = mx(af, ax) or 0
        f, x = seq(tree.body)
        out["outside"] = mx(f, x) or 0
    except Unbounded as e:
        out = {"error": str(e)}
else:
    try:
        f, x = seq(tree.body)
        out["outside"] = mx(f, x) or 0
    except Unbounded as e:
        out = {"error": str(e)}
print(json.dumps(out))
`

// campaignScriptWalls runs the analyzer on one of campaign's tool scripts.
func campaignScriptWalls(t *testing.T, node, mode string) scriptWalls {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 not on PATH under CI — the campaign's walls would go unread")
		}
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, node+".py")
	if err := os.WriteFile(script, []byte(toolScript(t, "campaign/main.bot", node)), 0o644); err != nil {
		t.Fatal(err)
	}
	analyzer := filepath.Join(dir, "walls.py")
	if err := os.WriteFile(analyzer, []byte(wallsAnalyzer), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("python3", "-I", analyzer, script, mode).Output() // #nosec G204 -- test-only, both paths are the test's
	if err != nil {
		t.Fatalf("reading %s's walls: %v", node, err)
	}
	var out struct {
		scriptWalls
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("reading %s's walls: %v (%s)", node, err, raw)
	}
	if out.Error != "" {
		t.Fatalf("campaign's %s has a command no wall bounds: %s — nothing then bounds the time it takes before the handoff", node, out.Error)
	}
	return out.scriptWalls
}

// afterLot is what follows a lot before the campaign reaches its handoff, at
// the walls of its commands: observe, then the steward outside its acts —
// which each start only where they end before its reserve — and the
// compute nodes after it, which run no command.
func afterLot(t *testing.T) time.Duration {
	t.Helper()
	observe := campaignScriptWalls(t, "observe", "total")
	steward := campaignScriptWalls(t, "steward", "steward")
	return time.Duration(observe.Outside+steward.Outside) * time.Second
}

// TestCampaignStewardReserveHoldsWhatAnActLeaves: an act starts only where
// its record and its counter-test — two commands, each under the lot's wall —
// end the steward's reserve (RESERVE_S) before 90% of the campaign's wall. So
// the reserve holds the rest of the act at its commands' walls, then the
// journal commit; finalize then starts before 90% of the wall, where the
// engine still starts it. A reserve shorter than that, or an act running a
// third command its start does not count, carries the campaign past that
// point and leaves it with no handoff.
func TestCampaignStewardReserveHoldsWhatAnActLeaves(t *testing.T) {
	t.Parallel()
	w := campaignScriptWalls(t, "steward", "steward")
	t.Logf("steward at its walls: %ds outside its acts, %ds of an act after its two commands, %ds of journal commit", w.Outside, w.Tail, w.After)
	if w.Records != 2 {
		t.Fatalf("an act runs %d commands under the lot's wall; its start counts two (the record, the counter-test) — revisit the start's check", w.Records)
	}
	if reserve := stewardReserve(t); reserve < time.Duration(w.Tail+w.After)*time.Second {
		t.Fatalf("the steward's reserve is %s: an act's own git work after its record and counter-test takes up to %ds at its walls, and the journal commit %ds after it — the campaign reaches 90%% of its wall before finalize starts, and ends with no handoff",
			reserve, w.Tail, w.After)
	}
}

// TestCampaignObserveWallsAreRead: the analyzer reads observe's walls — four
// commands under 60 s walls — and refuses a script whose command has none.
// What the bound on the room after a lot rests on is read, not assumed.
func TestCampaignObserveWallsAreRead(t *testing.T) {
	t.Parallel()
	w := campaignScriptWalls(t, "observe", "total")
	t.Logf("observe at its walls: %ds", w.Outside)
	if w.Outside <= 0 {
		t.Fatalf("observe's walls read as %ds: the reading sees none of its commands", w.Outside)
	}
	dir := t.TempDir()
	analyzer := filepath.Join(dir, "walls.py")
	if err := os.WriteFile(analyzer, []byte(wallsAnalyzer), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"no wall":      "import subprocess\nsubprocess.run(['git', 'status'])\n",
		"in a loop":    "import subprocess\nfor p in paths:\n    subprocess.run(['git', 'add', p], timeout=60)\n",
		"walled twice": "import subprocess\nsubprocess.run(['a'], timeout=60)\nif x:\n    subprocess.run(['b'], timeout=30)\nelse:\n    subprocess.run(['c'], timeout=90)\n",
	} {
		p := filepath.Join(dir, regexp.MustCompile(`\W`).ReplaceAllString(name, "_")+".py")
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		raw, err := exec.Command("python3", "-I", analyzer, p, "total").Output() // #nosec G204 -- test-only
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, raw)
		}
		_, refused := out["error"]
		switch name {
		case "walled twice":
			if refused || out["outside"] != 150.0 {
				t.Fatalf("%s reads %v, want 150 (60 then the longer branch)", name, out)
			}
		default:
			if !refused {
				t.Fatalf("a command %s reads %v, not as unbounded", name, out)
			}
		}
	}
}
