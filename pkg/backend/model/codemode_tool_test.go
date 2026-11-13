package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// childHost is a Host that re-execs THIS test binary as the script-host
// child. The arm that answers the subcommand is in TestMain
// (modelspecs_merge_test.go); production leaves Binary empty, which resolves
// the installed iterion.
//
// AllowSingleThreadConfinement is set, deliberately and only here. Whether
// this binary is cgo-linked is a property of the LEG (CI's unit leg builds
// CGO_ENABLED=0, its race shards need cgo), and a cgo-linked child can only
// confine the thread that asked — so without the allowance these tests would
// pass on one leg and be refused on another, for a reason that has nothing to
// do with what they measure. What they measure is the WIRING: that a
// program's calls reach the node's own door. The confinement posture itself
// is pkg/scripthost's to prove, against the real binary built the way iterion
// ships, where this allowance never applies — installCodemode leaves it
// false, and a child that could not confine its process refuses there.
func childHost() scripthost.Host {
	return scripthost.Host{Binary: os.Args[0], AllowSingleThreadConfinement: true}
}

// runCodemode drives the tool the way the generation loop does: the node's
// dispatcher is built over toolMap, and the tool's Execute is called with the
// context THAT dispatcher vouches for — which is the only context from which
// a program can reach a door at all.
func runCodemode(t *testing.T, toolMap map[string]*GenerationTool, source string) (string, error) {
	t.Helper()
	_, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)
	in, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		t.Fatal(err)
	}
	return codemodeTool(childHost(), scripthost.Limits{}).Execute(ctx, in)
}

// TestCodemodeCallsReachTheNodesOwnDoor is the whole slice in one assertion,
// and it is what the `codemode` capability cell cites as its evidence.
//
// It starts where the MODEL starts — a tool call whose input is a program —
// and ends where a direct call ends: the node's own tool ran, once, with the
// argument the program composed. Every link in between is exercised rather
// than described: the tool's input schema, the spawn of a confined child, the
// tool channel, the server built from the dispatch's context, the dispatcher,
// and the result travelling back as the program's last expression.
//
// A test that called scriptToolServer directly would prove the same middle
// and none of the ends — and the ends are where this slice's defects live.
func TestCodemodeCallsReachTheNodesOwnDoor(t *testing.T) {
	t.Parallel()
	var ran bool
	tools := bashTool(&ran)

	// The program loops, which is the point of codemode: three calls, one
	// returned line. A model doing this directly would carry three
	// tool_results in its context.
	got, err := runCodemode(t, tools, `
		const out = [];
		for (const c of ["one", "two", "three"]) {
			out.push(call("Bash", {command: c}).text);
		}
		out.join("+")
	`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !ran {
		t.Error("the node's own tool never ran: the program's calls did not reach the door")
	}
	// Host.Run returns the value JSON-encoded, which is what the model reads.
	if got != `"ran+ran+ran"` {
		t.Errorf("the tool returned %q, want the program's last expression `\"ran+ran+ran\"`", got)
	}
}

// TestAProgramCannotStartAProgram pins the ARGUMENT codemodeTool passes to
// newScriptToolServer. Passing "" there — the value CM-1b shipped, and the
// one a copy of the existing test would keep — compiles, passes every other
// test in this file, and lets a program spawn a child that spawns a child.
func TestAProgramCannotStartAProgram(t *testing.T) {
	t.Parallel()
	var ran bool
	tools := bashTool(&ran)
	tools[ScriptHostToolName] = &GenerationTool{
		Name: ScriptHostToolName,
		Execute: func(context.Context, json.RawMessage) (string, error) {
			t.Error("a nested script host was dispatched")
			return "", nil
		},
	}

	// The call THROWS inside the program, so the program catches it and
	// returns what it was told: a refusal the model can read, not a crash.
	got, err := runCodemode(t, tools, `
		try { call("`+ScriptHostToolName+`", {source: "1"}); "NOT REFUSED" }
		catch (e) { e.text }
	`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(got, "NOT REFUSED") {
		t.Fatalf("a program started a program: %s", got)
	}
	if !strings.Contains(got, ScriptHostToolName) {
		t.Errorf("the refusal does not name the tool it withheld: %s", got)
	}
}

// TestCodemodeWithoutTheNodesDoorRefuses holds the failure mode this slice
// exists to make impossible: a program served by something that is not the
// node's dispatcher. The context here carries none, so the tool refuses
// before a child is spawned rather than running a program with no door.
func TestCodemodeWithoutTheNodesDoorRefuses(t *testing.T) {
	t.Parallel()
	in, err := json.Marshal(map[string]string{"source": `1`})
	if err != nil {
		t.Fatal(err)
	}
	out, err := codemodeTool(childHost(), scripthost.Limits{}).Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("a program ran on a context with no dispatcher, returning %q", out)
	}
	if !strings.Contains(err.Error(), ScriptHostToolName) {
		t.Errorf("the refusal does not name the tool: %v", err)
	}
}

// TestARefusedProgramComesBackAsAToolError pins which of the two exits a
// bound takes. scripthost separates them on purpose — a Response refusal is
// the program's doing, a returned error is the deployment's — and the tool
// must turn the first into something the MODEL can act on, because the model
// is the only party that can write a smaller program. shapeToolOutcome makes
// a returned error an is_error tool_result the loop carries on from.
func TestARefusedProgramComesBackAsAToolError(t *testing.T) {
	t.Parallel()
	var ran bool
	out, err := runCodemode(t, bashTool(&ran), `throw new Error("MARKER-FROM-THE-PROGRAM")`)
	if err == nil {
		t.Fatalf("a throwing program returned %q and no error", out)
	}
	if !strings.Contains(err.Error(), "MARKER-FROM-THE-PROGRAM") {
		t.Errorf("the model is not told why its program was refused: %v", err)
	}
}

// TestTheCodemodeToolIsOnTheRequestOnlyWhenTheTaskAsksForIt witnesses the
// BRANCH in claw_backend.go, which no other test here can see: the tool is
// built and installed by two functions a unit test can call directly, and a
// mutant that drops the `if task.Codemode` guard — installing it on every
// node of the fleet — leaves all of those green.
//
// It reads the tool array of the request that actually went to the model,
// which is also what proves the tool is DECLARED rather than merely known.
func TestTheCodemodeToolIsOnTheRequestOnlyWhenTheTaskAsksForIt(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		backend, cap := newCapturingBackend()
		if _, err := backend.Execute(context.Background(), delegate.Task{
			NodeID:       "n",
			Model:        "test/test-model",
			SystemPrompt: "be terse",
			UserPrompt:   "x",
			Codemode:     on,
		}); err != nil {
			t.Fatalf("Execute(codemode=%v): %v", on, err)
		}
		if len(cap.requests) != 1 {
			t.Fatalf("requests = %d, want 1", len(cap.requests))
		}
		var names []string
		for _, tool := range cap.requests[0].Tools {
			names = append(names, tool.Name)
		}
		if got := slices.Contains(names, ScriptHostToolName); got != on {
			t.Errorf("task.Codemode=%v: the request offers %q = %v, want %v (tools: %v)",
				on, ScriptHostToolName, got, on, names)
		}
	}
}

// TestTwoDispatchesOfTheSameToolDoNotCrossDoors holds the property the tool's
// own comment asserts, and which a review found the code did not keep: ONE
// GenerationTool is installed per node and its Execute closure is shared by
// every call of it, so a `host` mutated in place is mutated for everyone.
//
// The crossing it guards against is the worst defect this slice could carry:
// a program dispatched on node A's context served through node B's door —
// B's permission policy, B's hooks, B's materialised secrets. The two tool
// maps here are DISJOINT, so a crossing shows up as a tool that does not
// exist rather than as a silent substitution; in production the maps overlap
// (`Bash`, `Read`), which is exactly why the test has to force the disjoint
// case to see it at all.
//
// It is also a `-race` witness: the shared write is a data race before the
// fix, and the race detector reports it at the assignment line.
func TestTwoDispatchesOfTheSameToolDoNotCrossDoors(t *testing.T) {
	t.Parallel()

	namedTool := func(name string, ran *atomic.Int64) map[string]*GenerationTool {
		return map[string]*GenerationTool{name: {
			Name: name,
			Execute: func(context.Context, json.RawMessage) (string, error) {
				ran.Add(1)
				return "served-by-" + name, nil
			},
		}}
	}

	// ONE tool, as installCodemode appends exactly one per node.
	// A bigger memory bound than the default, and only here: the child is
	// THIS test binary, and on the `-race` leg it is race-instrumented, which
	// does not fit in DefaultLimits' 256 MiB. A child dying of memory_exceeded
	// would be read by the assertion below as a crossing. 1 GiB is under
	// scripthost.Ceiling, so Clamp leaves it alone.
	tool := codemodeTool(childHost(), scripthost.Limits{Memory: 1 << 30})

	var ranA, ranB atomic.Int64
	_, ctxA := newToolDispatcher(context.Background(), namedTool("ToolA", &ranA), nil, nil, nil, nil, nil)
	_, ctxB := newToolDispatcher(context.Background(), namedTool("ToolB", &ranB), nil, nil, nil, nil, nil)

	source := func(name string) json.RawMessage {
		in, err := json.Marshal(map[string]string{"source": `call("` + name + `", {}).text`})
		if err != nil {
			t.Fatal(err)
		}
		return in
	}

	// Pairs are CONCURRENT but BOUNDED: every call spawns a child process
	// holding a 256 MiB RLIMIT_DATA, and an unbounded fan-out exhausted the
	// machine under `-race` — children then die of memory_exceeded, which
	// this test would have read as a crossing. The bound is what keeps the
	// red attributable to the defect.
	const (
		rounds   = 16
		inFlight = 3
	)
	type outcome struct {
		door string
		got  string
		err  error
	}
	results := make(chan outcome, 2*rounds)
	sem := make(chan struct{}, inFlight)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		sem <- struct{}{}
		wg.Add(2)
		go func() {
			defer wg.Done()
			got, err := tool.Execute(ctxA, source("ToolA"))
			results <- outcome{"A", got, err}
		}()
		go func() {
			defer func() { wg.Done(); <-sem }()
			got, err := tool.Execute(ctxB, source("ToolB"))
			results <- outcome{"B", got, err}
		}()
	}
	wg.Wait()
	close(results)

	crossings := 0
	for r := range results {
		want := `"served-by-Tool` + r.door + `"`
		if r.err != nil || r.got != want {
			crossings++
			if crossings == 1 {
				t.Errorf("a program dispatched on node %s's context was not served by node %s's door: got %q err=%v, want %q",
					r.door, r.door, r.got, r.err, want)
			}
		}
	}
	if crossings > 0 {
		t.Errorf("%d crossing(s) over %d concurrent pairs", crossings, rounds)
	}
}

// TestADeploymentFailureSaysItIsOne kills a mutant that survived the bench:
// deleting the clause that tells the model a host failure is not its
// program's fault changed nothing any test could see.
//
// scripthost separates the two classes on purpose — a Response refusal is the
// program's doing, a returned error is the deployment's — and both arrive at
// the model as an ordinary is_error tool_result, which shapeToolOutcome
// renders identically. The tool_result TEXT is therefore the only place that
// distinction can reach the one party that would otherwise rewrite the
// program until the node's step budget is spent. Measured by an adversarial
// round against a Host whose binary does not exist, which is what a pod or an
// image without iterion on PATH looks like.
func TestADeploymentFailureSaysItIsOne(t *testing.T) {
	t.Parallel()
	in, err := json.Marshal(map[string]string{"source": `1`})
	if err != nil {
		t.Fatal(err)
	}
	_, ctx := newToolDispatcher(context.Background(), map[string]*GenerationTool{}, nil, nil, nil, nil, nil)

	// A host that cannot start: the deployment class.
	broken := scripthost.Host{Binary: filepath.Join(t.TempDir(), "no-such-iterion")}
	_, hostErr := codemodeTool(broken, scripthost.Limits{}).Execute(ctx, in)
	if hostErr == nil {
		t.Fatal("a Host whose binary does not exist returned no error")
	}
	if !strings.Contains(hostErr.Error(), "DEPLOYMENT failure") {
		t.Errorf("the model is not told this is a deployment failure, so it rewrites the program until the step budget is gone: %v", hostErr)
	}

	// A program that throws: the program class, which the model CAN fix. It
	// must not carry the clause, or the clause means nothing.
	var ran bool
	_, progErr := runCodemode(t, bashTool(&ran), `throw new Error("MARKER")`)
	if progErr == nil {
		t.Fatal("a throwing program returned no error")
	}
	if strings.Contains(progErr.Error(), "DEPLOYMENT failure") {
		t.Errorf("a program's own error is labelled a deployment failure, which tells the model not to fix what it can fix: %v", progErr)
	}
}

// TestACancelledRunIsNotCalledADeploymentFailure is the third class the first
// draft of the clause above collapsed into the second. scripthost's own Run
// doc separates them — "the binary could not be started, the deployment
// answered nothing this code understands, or the caller's own context ended"
// — and a run that was cancelled, a node that hit its `timeout:`, or a
// duration budget that ran out is none of the deployment's doing.
//
// Telling a model "no other program will run either" when the run simply
// ended is a false sentence in the transcript and in the run record. A second
// adversarial round executed it.
func TestACancelledRunIsNotCalledADeploymentFailure(t *testing.T) {
	t.Parallel()
	in, err := json.Marshal(map[string]string{"source": `1`})
	if err != nil {
		t.Fatal(err)
	}
	_, base := newToolDispatcher(context.Background(), map[string]*GenerationTool{}, nil, nil, nil, nil, nil)

	// BOTH context errors, because the branch names both and the message
	// leads with the deadline: a one-row test built with WithCancel only ever
	// produces context.Canceled, and dropping `|| DeadlineExceeded` from the
	// branch left the whole package green.
	for name, ctx := range map[string]context.Context{
		"the run was cancelled": func() context.Context {
			c, cancel := context.WithCancel(base)
			cancel()
			return c
		}(),
		"the node's deadline passed": func() context.Context {
			c, cancel := context.WithDeadline(base, time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := codemodeTool(childHost(), scripthost.Limits{}).Execute(ctx, in)
			if err == nil {
				t.Fatal("an ended context returned no error")
			}
			if strings.Contains(err.Error(), "DEPLOYMENT failure") {
				t.Errorf("reported to the model as a broken deployment: %v", err)
			}
			if !strings.Contains(err.Error(), "deadline ended") {
				t.Errorf("does not say what actually happened: %v", err)
			}
		})
	}
}

// TestTheShippedCellsAreExactlyTheLoopsThatInstallTheTool closes the
// direction every other guard misses, and it is the DANGEROUS one.
//
// A registry cell carrying a wrong THIRD verdict (`partial`, `inert`, …)
// splits the compiler's predicate from the engine's and dies loud at
// dispatch. A cell carrying a wrong `shipped` says nothing at all: the
// compiler accepts, the executor grants, the task carries `Codemode: true`
// to a loop that never reads it, and the node runs without the capability its
// author declared — the silent degradation codemodePolicy exists to prevent.
// A closing adversarial round flipped claude_code's cell to `shipped`,
// regenerated the golden as any registry edit does, and watched
// pkg/dsl/ir, pkg/backend/backendcaps and pkg/backend/model all stay green.
//
// So the claim is pinned at BOTH ends: the set of cells that say shipped, and
// the set of generation loops that actually call installCodemode — the
// second read from the source with an AST walk rather than from a list, so a
// second loop cannot appear without this test seeing it.
func TestTheShippedCellsAreExactlyTheLoopsThatInstallTheTool(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, top := range []string{"pkg", "cmd", "internal"} {
		walkErr := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == ".works" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Contains(src, []byte("installCodemode(")) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "installCodemode" {
					callers = append(callers, filepath.ToSlash(rel))
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", top, walkErr)
		}
	}
	slices.Sort(callers)
	callers = slices.Compact(callers)
	if len(callers) == 0 {
		t.Fatal("the sweep found no installCodemode call at all — the predicate stopped matching, and this test is blind")
	}
	if want := []string{"pkg/backend/model/claw_backend.go"}; !slices.Equal(callers, want) {
		t.Errorf("installCodemode is called from %v, want %v — a new install site must change a registry cell in the same commit", callers, want)
	}

	// The registry's side of the same fact. The two sets are named
	// separately and compared, rather than one being derived from the other:
	// a derivation would make this test agree with itself.
	if got, want := ir.CodemodeServingBackends(), []string{backendcaps.Claw}; !slices.Equal(got, want) {
		t.Errorf("the registry says %v serve codemode, but only the claw generation loop installs the tool: a `shipped` cell on %v hands those nodes a Codemode no loop reads", got, got)
	}
}

// TestCodemodeReachesTheWireOnEveryGenerationStrategy is the GUARANTEE behind
// TestTheCodemodeToolIsOnTheRequestOnlyWhenTheTaskAsksForIt, which certified
// the SITE and not the guarantee: it built a task with no OutputSchema, so it
// exercised one of the three strategy arms — the one where the property held.
//
// `ClawBackend.Execute` picks between three generation strategies on
// (hasSchema, HasTools), and the structured one NILS the tool list on
// purpose: `GenerateObjectDirect` puts only its synthetic `structured_output`
// tool on the wire. A node with an `output:` schema and no `tools:` therefore
// installed the codemode tool and sent a request without it — the author
// asked for the capability, the compiler said yes, the executor said yes, and
// nothing in the run said otherwise. That is the silent degradation this
// whole slice exists to prevent, reached through the dispatch instead of
// through the registry, and an adversarial round found it after three rounds
// and four mutation benches had not.
//
// The test is a matrix over the three arms because the defect lived in the
// one arm the single-case test did not take.
func TestCodemodeReachesTheWireOnEveryGenerationStrategy(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"}}}`)
	for _, c := range []struct {
		name     string
		schema   json.RawMessage
		hasTools bool
	}{
		{"no schema, no declared tools", nil, false},
		{"a schema and no declared tools", schema, false},
		{"a schema and declared tools", schema, true},
		{"declared tools, no schema", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			backend, cap := newCapturingBackend()
			task := delegate.Task{
				NodeID:       "n",
				Model:        "test/test-model",
				SystemPrompt: "be terse",
				UserPrompt:   "go",
				OutputSchema: c.schema,
				HasTools:     c.hasTools,
				Codemode:     true,
			}
			if c.hasTools {
				task.AllowedTools = []string{"Bash"}
			}
			if _, err := backend.Execute(context.Background(), task); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(cap.requests) == 0 {
				t.Fatal("no request reached the wire")
			}
			var names []string
			for _, tool := range cap.requests[0].Tools {
				names = append(names, tool.Name)
			}
			if !slices.Contains(names, ScriptHostToolName) {
				t.Errorf("a Codemode:true node reached the wire WITHOUT the %q tool (tools: %v): the capability its author declared was dropped in silence",
					ScriptHostToolName, names)
			}
			// The half a narrower fix gets wrong. Moving the node onto the
			// tools+schema strategy without the OUTPUT FORMAT instruction
			// leaves it on an arm that DEPENDS on that instruction to get
			// its JSON — the request's user text would have been just the
			// prompt. The instruction is keyed on the same flag, so the
			// assignment has to land before its reader, not merely before
			// the strategy switch.
			if c.schema != nil {
				var text string
				for _, m := range cap.requests[0].Messages {
					for _, b := range m.Content {
						text += b.Text
					}
				}
				if !strings.Contains(text, "OUTPUT FORMAT") {
					t.Errorf("a schema-bearing node on the tool loop never got its OUTPUT FORMAT instruction; the user text was %q", text)
				}
			}
		})
	}
}

// TestAProgramCannotPauseTheRun holds the plainest sentence ADR-165 writes
// about what a program may not do. A fifth adversarial round reached the
// ASYNC half of the ask family from inside a program: `withhold` asked
// permission.IsInfrastructureTool, which matches `ask_user` by name and
// everything else by the `mcp_iterion` namespace — and claw registers
// `ask_user_async` and `await_answers` as NATIVE tools, whose bare names
// carry no namespace at all.
//
// The matrix is built from the delegate CONSTANTS, so a fourth member of the
// family cannot be added without this test seeing it, and it ends with the
// positive arm: an ordinary tool still reaches the node's door, or the
// withholding would be proven by a server that refuses everything.
func TestAProgramCannotPauseTheRun(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ask_user", delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName, "send_user_message", "structured_output", "agent", "task", ScriptHostToolName} {
		t.Run(name, func(t *testing.T) {
			var ran bool
			tools := bashTool(&ran)
			tools[name] = &GenerationTool{
				Name: name,
				Execute: func(context.Context, json.RawMessage) (string, error) {
					t.Errorf("%q was dispatched from inside a program", name)
					return "DISPATCHED", nil
				},
			}
			got, err := runCodemode(t, tools, `
				try { call(`+strconv.Quote(name)+`, {}); "NOT REFUSED" }
				catch (e) { e.text }
			`)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if strings.Contains(got, "NOT REFUSED") || strings.Contains(got, "DISPATCHED") {
				t.Fatalf("a program reached %q: %s", name, got)
			}
			if !strings.Contains(got, name) {
				t.Errorf("the refusal does not name the tool it withheld: %s", got)
			}
		})
	}

	t.Run("an ordinary tool still reaches the door", func(t *testing.T) {
		var ran bool
		got, err := runCodemode(t, bashTool(&ran), `call("Bash", {command: "x"}).text`)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !ran || got != `"ran"` {
			t.Errorf("the node's own tool did not run (ran=%v, got=%q): a server that refuses everything would pass every case above", ran, got)
		}
	})
}

// TestAnAskVerdictInsideAProgramDegradesRatherThanPausing holds the OTHER
// half of the rule an earlier round shipped only half of.
//
// Withholding the ask TOOLS stops a program from asking on purpose. It does
// nothing about the ask VERDICT, which an ORDINARY tool raises from inside a
// program the moment a permission rule matches it — and a sixth adversarial
// round drove exactly that: `call("Bash", …)` under an `ask` policy raised
// *delegate.ErrAskUser out of the codemode tool, tore the program down with
// its earlier calls already committed, and parked the pause on a `codemode`
// tool_use no resume can return to.
//
// subagent.go states the rule in two parts for the identical problem (#2877)
// and implements both; this is the second. The degradation is what the
// withholding's own refusal text promises: call it directly instead.
func TestAnAskVerdictInsideAProgramDegradesRatherThanPausing(t *testing.T) {
	t.Parallel()
	// `one` is allowed outright; `two` matches the ask rule. The program
	// calls both, in that order, so the test also sees what a pause would
	// have cost: the first call's effect is already committed.
	pol, err := permission.NewPolicy(permission.ModeAsk, []string{"Bash(one:*)"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pol.Enabled() || !pol.CanAsk() {
		t.Fatalf("the fixture's policy cannot ask (enabled=%v canAsk=%v): this test would pass over the thing it holds", pol.Enabled(), pol.CanAsk())
	}

	var ran atomic.Int64
	tools := map[string]*GenerationTool{"Bash": {
		Name: "Bash",
		Execute: func(_ context.Context, in json.RawMessage) (string, error) {
			ran.Add(1)
			return "ran:" + string(in), nil
		},
	}}
	_, ctx := newToolDispatcher(context.Background(), tools, nil, nil, nil, nil, pol)
	in, err := json.Marshal(map[string]string{"source": `
		const out = [];
		out.push(call("Bash", {command: "one"}).text);
		try { out.push(call("Bash", {command: "two"}).text) } catch (e) { out.push("REFUSED:" + e.text) }
		out.join("|")
	`})
	if err != nil {
		t.Fatal(err)
	}

	got, execErr := codemodeTool(childHost(), scripthost.Limits{Memory: 1 << 30}).Execute(ctx, in)

	var ask *delegate.ErrAskUser
	if errors.As(execErr, &ask) {
		t.Fatalf("a program paused the run (pending tool_use %q): the ask verdict must degrade to a refusal the program can catch, never a pause no resume can return to", ask.PendingToolUseID)
	}
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if !strings.Contains(got, "REFUSED:") {
		t.Errorf("the asked call did not come back as a catchable refusal: %s", got)
	}
	if !strings.Contains(got, "ran:") {
		t.Errorf("the allowed call did not run: %s", got)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the node's tool ran %d time(s), want 1 — the allowed call only", n)
	}
}

// TestTheToolUseNudgeReachesACodemodeNodeWithNoDeclaredTools witnesses the
// FOURTH reader of NeedsToolLoop. A sixth adversarial round reverted that one
// site to `task.HasTools` and the whole package stayed green: the OUTPUT
// FORMAT instruction and the strategy switch had witnesses, the nudge did not.
//
// The line is load-bearing for exactly the shape this slice creates — a
// schema-bearing node whose only tool is codemode, which narrates instead of
// acting. Without it that node skips the one cheap nudge turn and falls to
// the schema re-ask, which turns codemode off for the retry.
func TestTheToolUseNudgeReachesACodemodeNodeWithNoDeclaredTools(t *testing.T) {
	t.Parallel()
	// Asserted on the nudge's own SIGNATURE rather than on a turn count:
	// the structured path makes turns of its own, and a count would be a
	// number fitted to an observation instead of a property.
	const marker = "You ended without using any of your available tools"
	for _, c := range []struct {
		name      string
		codemode  bool
		wantNudge bool
	}{
		{"a codemode node that used no tool is nudged", true, true},
		{"a node with neither tools nor codemode is left alone", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			backend, cap := newCapturingBackend()
			if _, err := backend.Execute(context.Background(), delegate.Task{
				NodeID:       "n",
				Model:        "test/test-model",
				UserPrompt:   "go",
				OutputSchema: json.RawMessage(`{"type":"object"}`),
				HasTools:     false,
				Codemode:     c.codemode,
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			nudged := false
			for _, req := range cap.requests {
				for _, m := range req.Messages {
					for _, block := range m.Content {
						if strings.Contains(block.Text, marker) {
							nudged = true
						}
					}
				}
			}
			if nudged != c.wantNudge {
				t.Errorf("codemode=%v: the tool-use nudge fired=%v, want %v (over %d request(s))",
					c.codemode, nudged, c.wantNudge, len(cap.requests))
			}
		})
	}
}
