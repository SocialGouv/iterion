package liveledger

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/liveenv"
	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// liveKnobsFile is the e2e file that declares the launch-knob chokepoint.
// Untagged on purpose (the free suite drives it), so a scan of the live
// harness reads it beside the live-tagged files.
const liveKnobsFile = "live_knobs_test.go"

// liveHarnessSources reads the live harness from the tree the test process
// runs in (sweepwalk.ModuleRoot — the working directory, never
// runtime.Caller): every live-tagged e2e test file, build tag included, plus
// the untagged knob file the live build compiles with them. An empty or
// mis-filtered set is refused: a scanner over nothing is green on nothing.
func liveHarnessSources(t *testing.T) map[string]string {
	t.Helper()
	sources, err := readLiveTestSources(filepath.Join(sweepwalk.ModuleRoot(t), "e2e"))
	if err != nil {
		t.Fatalf("read the e2e test sources: %v", err)
	}
	out := map[string]string{}
	for name, src := range sources {
		if strings.Contains(src, "//go:build live") || name == liveKnobsFile {
			out[name] = src
		}
	}
	if _, ok := out[liveKnobsFile]; !ok {
		t.Fatalf("e2e/%s is missing — the file that declares the launch-knob chokepoint", liveKnobsFile)
	}
	if len(out) < 31 {
		t.Fatalf("only %d live harness file(s) found in e2e/ — the build-tag filter is broken, and a small set would make every rule pass vacuously", len(out))
	}
	return out
}

// The gestures the sweep reads in a live harness function. Each is a way a
// launch knob reaches — or bypasses — a run.
const (
	gestureExecutor      = "builds a ClawExecutor (model.NewClawExecutor)"
	gestureEngine        = "builds an engine (runtime.New / runtime.NewFromRecipe)"
	gestureBudget        = "applies a budget override (ir.ApplyBudgetOverrides)"
	gestureKnobRead      = "reads the launch knobs (liveenv.Read, or os.Getenv / os.LookupEnv of a knob)"
	gestureOverride      = "attaches a model override (model.WithModelOverrides)"
	gestureProvenance    = "tells a ledger tracker what served (SetProvenance)"
	gestureSubagent      = "hands a subagent runner a default model the re-target cannot reach (neither \"\" nor liveSubagentDefault)"
	gestureSeamless      = "launches a run outside the engine (cli.RunScheduleRun / cli.RunRun / cli.RunResume / a delegate.Task)"
	gestureBudgetSubject = "asserts on its fixture's own budget (store.EventBudgetExceeded)"
	gestureTrack         = "records a ledger row (liveledger.Track)"
)

// ownedGestures are the gestures exactly ONE function of the harness may
// make, exactly as many times: the chokepoint. A second site is a knob path
// beside it — the class every high finding of #1423's review rounds belonged
// to — and a count of zero is a chokepoint that moved, was renamed or
// stopped applying its knob.
var ownedGestures = []struct {
	gesture, owner string
	want           int
}{
	{gestureExecutor, "newLiveClawExecutor", 1},
	{gestureEngine, "newLiveEngine", 1},
	{gestureBudget, "armLiveRun", 1},
	{gestureOverride, "armLiveRun", 1},
	{gestureKnobRead, "liveOverride", 1},
	{gestureProvenance, "recordLiveProvenance", 1},
}

// liveSeamlessExempt names the harness functions that launch a model call
// outside the engine and are NOT a live target's run, each with its reason.
// A decision on the record: the entry must keep matching a real launch, or
// the sweep calls it stale.
var liveSeamlessExempt = map[string]string{
	"liveJudgeInvoker": "the quality panel's judge grades the target's artifact after the run; the launch knobs re-target and cap the target's run, and the panel is governed by its own knobs (ITERION_LIVE_QUALITY, ITERION_LIVE_JUDGE_MODELS)",
}

// harnessFunc is what the sweep knows about one function of the harness:
// the plain identifiers its body calls (closures included — a subtest's body
// is its test's) and the lines where it makes each gesture.
type harnessFunc struct {
	file     string
	name     string
	calls    map[string]bool
	gestures map[string][]int
}

// scanHarness parses every source and records, per declared function, its
// calls and gestures. It reads the SOURCE rather than text: a call in a
// comment or a string satisfies a grep, and one split across lines defeats a
// line-oriented one.
func scanHarness(sources map[string]string) (map[string]*harnessFunc, error) {
	out := map[string]*harnessFunc{}
	for name, src := range sources {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse e2e/%s: %w", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				key = types0(fn.Recv.List[0].Type) + "." + key
			}
			h := &harnessFunc{file: name, name: key, calls: map[string]bool{}, gestures: map[string][]int{}}
			add := func(g string, n ast.Node) { h.gestures[g] = append(h.gestures[g], fset.Position(n.Pos()).Line) }
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						h.calls[id.Name] = true
					}
					switch {
					case isSelectorCall(x, "model", "NewClawExecutor"):
						add(gestureExecutor, x)
					case isSelectorCall(x, "runtime", "New"), isSelectorCall(x, "runtime", "NewFromRecipe"):
						add(gestureEngine, x)
					case isSelectorCall(x, "ir", "ApplyBudgetOverrides"):
						add(gestureBudget, x)
					case isSelectorCall(x, "liveenv", "Read"):
						add(gestureKnobRead, x)
					case isSelectorCall(x, "os", "Getenv"), isSelectorCall(x, "os", "LookupEnv"):
						if len(x.Args) == 1 && namesLaunchKnob(x.Args[0]) {
							add(gestureKnobRead, x)
						}
					case isSelectorCall(x, "model", "WithModelOverrides"):
						add(gestureOverride, x)
					case isSelectorCall(x, "model", "NewSubagentRunner"):
						if pinsSubagentDefault(x) {
							add(gestureSubagent, x)
						}
					case isSelectorCall(x, "cli", "RunScheduleRun"), isSelectorCall(x, "cli", "RunRun"), isSelectorCall(x, "cli", "RunResume"):
						add(gestureSeamless, x)
					case isTrackCall(x):
						add(gestureTrack, x)
					}
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetProvenance" {
						add(gestureProvenance, x)
					}
				case *ast.CompositeLit:
					if isPkgSelector(x.Type, "delegate", "Task") {
						add(gestureSeamless, x)
					}
				case *ast.SelectorExpr:
					if isPkgSelector(x, "store", "EventBudgetExceeded") {
						add(gestureBudgetSubject, x)
					}
				}
				return true
			})
			out[key] = h
		}
	}
	return out, nil
}

// types0 names a method receiver's type for the function key, so a method
// never shadows a function of the same name.
func types0(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return types0(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return types0(x.X)
	}
	return "?"
}

// isPkgSelector reports whether e is `<pkg>.<sel>`.
func isPkgSelector(e ast.Expr, pkg, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// namesLaunchKnob reports whether e names one of the harness's launch knobs:
// the literal name, or liveenv's constant for it.
func namesLaunchKnob(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		v, err := strconv.Unquote(x.Value)
		return err == nil && slices.Contains(liveenv.LaunchKnobs(), v)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "liveenv" && strings.HasPrefix(x.Sel.Name, "Env")
	}
	return false
}

// pinsSubagentDefault reports whether a model.NewSubagentRunner call hands
// its children a default model the re-target cannot reach: anything but the
// empty literal (the child inherits its node's model) or a call to
// liveSubagentDefault (the re-target replaces the pinned default).
func pinsSubagentDefault(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	switch last := call.Args[len(call.Args)-1].(type) {
	case *ast.BasicLit:
		return last.Value != `""`
	case *ast.CallExpr:
		id, ok := last.Fun.(*ast.Ident)
		return !ok || id.Name != "liveSubagentDefault"
	}
	return true
}

// reaches reports whether from calls target, directly or through the
// harness's own functions.
func reaches(funcs map[string]*harnessFunc, from, target string) bool {
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		f := funcs[queue[0]]
		queue = queue[1:]
		if f == nil {
			continue
		}
		if f.calls[target] {
			return true
		}
		for callee := range f.calls {
			if !seen[callee] {
				seen[callee] = true
				queue = append(queue, callee)
			}
		}
	}
	return false
}

// knobChokepointViolations applies the rules to a scanned harness. Sorted,
// so a message never depends on the map order the scan produced.
func knobChokepointViolations(funcs map[string]*harnessFunc, exempt map[string]string) []string {
	var out []string
	for _, r := range ownedGestures {
		inside := 0
		for _, f := range funcs {
			lines := f.gestures[r.gesture]
			if f.name == r.owner {
				inside += len(lines)
				continue
			}
			for _, l := range lines {
				out = append(out, fmt.Sprintf("e2e/%s:%d (%s) %s outside %s — every launch knob reaches a run there and nowhere else", f.file, l, f.name, r.gesture, r.owner))
			}
		}
		if inside != r.want {
			out = append(out, fmt.Sprintf("%s %s %d time(s), want exactly %d — the chokepoint moved, was renamed, was duplicated or stopped applying its knob", r.owner, r.gesture, inside, r.want))
		}
	}
	for _, f := range funcs {
		for _, l := range f.gestures[gestureSubagent] {
			out = append(out, fmt.Sprintf("e2e/%s:%d (%s) %s — pass liveSubagentDefault(t, <pinned>) or \"\"", f.file, l, f.name, gestureSubagent))
		}
		if lines := f.gestures[gestureSeamless]; len(lines) > 0 && !f.calls["refuseRetargetWithoutSeam"] {
			if _, ok := exempt[f.name]; !ok {
				out = append(out, fmt.Sprintf("e2e/%s:%d (%s) %s and never calls refuseRetargetWithoutSeam — a re-target or a ceiling would be dropped while the run spends on the fixture's pinned model", f.file, lines[0], f.name, gestureSeamless))
			}
		}
		if lines := f.gestures[gestureBudgetSubject]; len(lines) > 0 && !f.calls["refuseCapOnBudgetSubject"] {
			out = append(out, fmt.Sprintf("e2e/%s:%d (%s) %s and never calls refuseCapOnBudgetSubject — ITERION_LIVE_MAX_COST_USD replaces that budget at the chokepoint, and the healthy target records a `fail`", f.file, lines[0], f.name, gestureBudgetSubject))
		}
		if lines := f.gestures[gestureTrack]; len(lines) > 0 && !reaches(funcs, f.name, "newLiveEngine") && !f.calls["refuseRetargetWithoutSeam"] {
			out = append(out, fmt.Sprintf("e2e/%s:%d (%s) %s but reaches no engine through newLiveEngine and refuses no knob — the run it records obtained none of the launch knobs", f.file, lines[0], f.name, gestureTrack))
		}
	}
	for name := range exempt {
		if f := funcs[name]; f == nil || len(f.gestures[gestureSeamless]) == 0 {
			out = append(out, fmt.Sprintf("the seamless-launch exemption for %s matches no launch any more — drop it, so the next launch there is judged", name))
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryLiveRunIsArmedAtTheKnobChokepoint is the guarantee behind every
// launch knob of the live harness (internal/liveenv). Three review rounds of
// #1423 found the same class twice: a knob applied on the runBotLive path
// while the targets that build their own executor or engine bypassed it — a
// re-target that never reached eight executors, a ceiling validated and then
// dropped on sixteen direct targets. The sweep holds the class for the whole
// harness, on the source of the files only a paid run executes:
//
//   - each knob-applying gesture (building an executor or an engine,
//     applying a budget, attaching the override, reading the knobs, feeding
//     provenance) is made by exactly one chokepoint function;
//   - a run launched outside the engine (a CLI command, a delegate backend
//     driven directly) refuses the run knobs; a test asserting on its own
//     fixture's budget refuses the ceiling;
//   - every function that records a ledger row reaches the engine
//     chokepoint, or refuses the knobs it cannot honour.
//
// TestEveryLaunchKnobReachesEveryLiveTargetShape (e2e, free) then proves the
// chokepoint applies each knob, on a real run.
func TestEveryLiveRunIsArmedAtTheKnobChokepoint(t *testing.T) {
	funcs, err := scanHarness(liveHarnessSources(t))
	if err != nil {
		t.Fatalf("scan the live harness: %v", err)
	}
	// Anti-vacuity: the scan must see the shapes it judges.
	tracked, seamless, subjects := 0, 0, 0
	for _, f := range funcs {
		if len(f.gestures[gestureTrack]) > 0 {
			tracked++
		}
		if len(f.gestures[gestureSeamless]) > 0 {
			seamless++
		}
		if len(f.gestures[gestureBudgetSubject]) > 0 {
			subjects++
		}
	}
	if tracked < 20 || seamless < 3 || subjects < 2 {
		t.Fatalf("the scan saw %d recording function(s), %d seamless launch(es) and %d budget subject(s) — fewer than the harness holds, so the rules below would pass vacuously", tracked, seamless, subjects)
	}
	if !reaches(funcs, "runBotLive", "newLiveEngine") {
		t.Fatal("runBotLive is not seen reaching newLiveEngine — the harness path most targets take, which the reachability rule is calibrated on")
	}
	if v := knobChokepointViolations(funcs, liveSeamlessExempt); len(v) > 0 {
		t.Fatalf("%d live harness site(s) bypass the launch-knob chokepoint (e2e/%s):\n  - %s", len(v), liveKnobsFile, strings.Join(v, "\n  - "))
	}
}

// The sweep's own table: a clean harness passes, each planted bypass is
// named once with its site, and a harness that lost its chokepoint is told
// so — so a rule that blesses everything reddens here, before the real tree.
func TestKnobChokepointViolations(t *testing.T) {
	chokepoint := `package e2e

func liveOverride(t *testing.T) liveenv.Override { ov, _ := liveenv.Read(os.Getenv); return ov }
func armLiveRun(t *testing.T, wf *ir.Workflow) []model.ClawExecutorOption {
	ir.ApplyBudgetOverrides(wf, bo)
	return []model.ClawExecutorOption{model.WithModelOverrides(mo)}
}
func newLiveClawExecutor(t *testing.T, reg *model.Registry, wf *ir.Workflow) *model.ClawExecutor {
	armLiveRun(t, wf)
	return model.NewClawExecutor(reg, wf)
}
func newLiveEngine(t *testing.T, wf *ir.Workflow, s store.RunStore, ex *model.ClawExecutor) *runtime.Engine {
	return runtime.New(wf, s, ex)
}
func recordLiveProvenance(tr *liveledger.Tracker, ov liveenv.Override, m string, served []string) {
	tr.SetProvenance(m, ov.FundedByFor(served))
}
func runBotLive(t *testing.T, spec liveSpec) liveResult {
	liveledger.Track(t)
	return liveResult{eng: newLiveEngine(t, wf, s, newLiveClawExecutor(t, reg, wf))}
}
func liveJudgeInvoker() { cc.Execute(ctx, delegate.Task{NodeID: "quality_judge"}) }
`
	clean := `//go:build live

package e2e

func TestLive_ViaHarness(t *testing.T) { runBotLive(t, liveSpec{}) }
func TestLive_ViaHelper(t *testing.T) { liveledger.Track(t); runDirect(t) }
func runDirect(t *testing.T) { _ = newLiveEngine(t, wf, s, newLiveClawExecutor(t, reg, wf)) }
func TestLive_Refused(t *testing.T) {
	tracker := liveledger.Track(t)
	refuseRetargetWithoutSeam(t, "delegate")
	_ = delegate.Task{NodeID: "x"}
	recordLiveProvenance(tracker, liveOverride(t), "", nil)
}
func TestLive_BudgetRefused(t *testing.T) {
	refuseCapOnBudgetSubject(t)
	res := runBotLive(t, liveSpec{})
	_ = hasEventType(res.events, store.EventBudgetExceeded)
}
func TestLive_Children(t *testing.T) {
	_ = model.NewSubagentRunner(reg, toolReg, hooks, nil, "")
	_ = model.NewSubagentRunner(reg, toolReg, hooks, nil, liveSubagentDefault(t, "anthropic/pinned"))
}
`
	funcs, err := scanHarness(map[string]string{"live_knobs_test.go": chokepoint, "clean_live_test.go": clean})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v := knobChokepointViolations(funcs, liveSeamlessExempt); len(v) != 0 {
		t.Fatalf("a clean harness was refused:\n  - %s", strings.Join(v, "\n  - "))
	}

	planted := `//go:build live

package e2e

func TestLive_OwnExecutor(t *testing.T) { liveledger.Track(t); _ = model.NewClawExecutor(reg, wf) }
func TestLive_OwnEngine(t *testing.T) {
	t.Run("sub", func(t *testing.T) { _ = runtime.New(wf, s, ex) })
}
func TestLive_OwnBudget(t *testing.T) { ir.ApplyBudgetOverrides(wf, bo) }
func TestLive_OwnRead(t *testing.T) { _, _ = liveenv.Read(os.Getenv) }
func TestLive_OwnGetenv(t *testing.T) { _ = os.Getenv("ITERION_LIVE_MAX_COST_USD") }
func TestLive_OwnLookup(t *testing.T) { _, _ = os.LookupEnv(liveenv.EnvModel) }
func TestLive_OtherEnv(t *testing.T) { _ = os.Getenv("ITERION_LIVE_CLAUDE_MODEL") }
func TestLive_OwnOverride(t *testing.T) { _ = model.WithModelOverrides(mo) }
func TestLive_OwnProvenance(t *testing.T) { tracker.SetProvenance("", "") }
func TestLive_PinnedChild(t *testing.T) { _ = model.NewSubagentRunner(reg, toolReg, hooks, nil, "anthropic/pinned") }
func TestLive_Scheduled(t *testing.T) { _ = cli.RunScheduleRun(ctx, p, opts) }
func TestLive_Delegated(t *testing.T) { _ = delegate.Task{NodeID: "x"} }
func TestLive_BudgetSubject(t *testing.T) { _ = store.EventBudgetExceeded }
func TestLive_TrackedNoRun(t *testing.T) { liveledger.Track(t) }
`
	funcs, err = scanHarness(map[string]string{"live_knobs_test.go": chokepoint, "planted_live_test.go": planted})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := knobChokepointViolations(funcs, liveSeamlessExempt)
	want := []string{
		"(TestLive_OwnExecutor) " + gestureExecutor + " outside newLiveClawExecutor",
		"(TestLive_OwnEngine) " + gestureEngine + " outside newLiveEngine",
		"(TestLive_OwnBudget) " + gestureBudget + " outside armLiveRun",
		"(TestLive_OwnRead) " + gestureKnobRead + " outside liveOverride",
		"(TestLive_OwnGetenv) " + gestureKnobRead + " outside liveOverride",
		"(TestLive_OwnLookup) " + gestureKnobRead + " outside liveOverride",
		"(TestLive_OwnOverride) " + gestureOverride + " outside armLiveRun",
		"(TestLive_OwnProvenance) " + gestureProvenance + " outside recordLiveProvenance",
		"(TestLive_PinnedChild) " + gestureSubagent,
		"(TestLive_Scheduled) " + gestureSeamless + " and never calls refuseRetargetWithoutSeam",
		"(TestLive_Delegated) " + gestureSeamless + " and never calls refuseRetargetWithoutSeam",
		"(TestLive_BudgetSubject) " + gestureBudgetSubject + " and never calls refuseCapOnBudgetSubject",
		"(TestLive_TrackedNoRun) " + gestureTrack + " but reaches no engine",
		"(TestLive_OwnExecutor) " + gestureTrack + " but reaches no engine",
	}
	for _, w := range want {
		n := 0
		for _, v := range got {
			if strings.Contains(v, w) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("planted bypass %q named %d time(s), want once; violations:\n  - %s", w, n, strings.Join(got, "\n  - "))
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d violation(s) for %d planted bypass(es) — a rule fired on a legal site or missed one:\n  - %s", len(got), len(want), strings.Join(got, "\n  - "))
	}

	// A harness that lost its chokepoint is refused on each owner's count,
	// and an exemption that matches no launch is stale.
	funcs, err = scanHarness(map[string]string{"clean_live_test.go": "//go:build live\n\npackage e2e\n\nfunc TestLive_Nothing(t *testing.T) {}\n"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got = knobChokepointViolations(funcs, liveSeamlessExempt)
	for _, r := range ownedGestures {
		found := false
		for _, v := range got {
			if strings.HasPrefix(v, r.owner+" "+r.gesture+" 0 time(s)") {
				found = true
			}
		}
		if !found {
			t.Errorf("a harness without %s was not refused on %q:\n  - %s", r.owner, r.gesture, strings.Join(got, "\n  - "))
		}
	}
	stale := false
	for _, v := range got {
		if strings.Contains(v, "exemption for liveJudgeInvoker matches no launch") {
			stale = true
		}
	}
	if !stale {
		t.Errorf("a stale exemption went unreported:\n  - %s", strings.Join(got, "\n  - "))
	}
}
