package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/hooks"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// What a run's events say about a codemode program (#3451): the program's
// nested calls name it as their parent, carry their program-order id and
// their batch slot, and the program's own completion event carries its
// bounds and verdict. Every witness here reads the events the real store
// hooks emit from the real dispatcher, driven by a real script-host child.

// nestedID matches a nested call's tool_use id and captures its frame.
var nestedID = regexp.MustCompile(`^codemode_[A-Z2-7]{26}_([0-9]+)$`)

// inspectSetup is one node: its tools, its declared codemode bounds, and the
// optional pieces a scenario adds.
type inspectSetup struct {
	tools  []GenerationTool
	want   scripthost.Limits
	guard  *secretguard.Guard
	state  CodemodeState
	runner *hooks.Runner
	host   *scripthost.Host
	// onEvent sees each event as it is appended, under the rig's lock.
	onEvent func(store.Event)
}

type inspectRig struct {
	mu     sync.Mutex
	events []store.Event
	opts   GenerationOptions
}

func newInspectRig(t *testing.T, s inspectSetup) *inspectRig {
	t.Helper()
	r := &inspectRig{}
	emitter := codemodeEventEmitter(func(e store.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
		if s.onEvent != nil {
			s.onEvent(e)
		}
	})
	r.opts = GenerationOptions{Tools: s.tools, codemodeState: s.state, Hooks: s.runner}
	applyHooks("node", 0, NewStoreEventHooks(context.Background(), emitter, "run", iterlog.Nop(), s.guard, nil), &r.opts)
	host := childHost()
	if s.host != nil {
		host = *s.host
	}
	want := s.want
	if err := installCodemodeOn(&r.opts, host, false, &want, nil); err != nil {
		t.Fatal(err)
	}
	return r
}

// program is one codemode tool_use block of a step.
type program struct{ id, source string }

// run dispatches the programs as ONE step's tool_use blocks, through the
// node's door, exactly as the generation loop does.
func (r *inspectRig) run(t *testing.T, ctx context.Context, programs ...program) {
	t.Helper()
	blocks := make([]toolUseBlock, 0, len(programs))
	for _, p := range programs {
		in, err := json.Marshal(map[string]string{"source": p.source})
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, toolUseBlock{ID: p.id, Name: ScriptHostToolName, PartialJSON: string(in)})
	}
	if _, err := executeToolsDirect(ctx, blocks, buildToolMap(r.opts.Tools), r.opts.OnToolStarted, r.opts.OnToolCall,
		r.opts.Hooks, nil, nil, r.opts.toolObserversAudited); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func (r *inspectRig) snapshot() []store.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func dataString(e store.Event, key string) string {
	s, _ := e.Data[key].(string)
	return s
}

// nestedFrame is the frame a nested event's id names, or -1 for an id that
// is not a nested one.
func nestedFrame(e store.Event) int64 {
	m := nestedID.FindStringSubmatch(dataString(e, "tool_use_id"))
	if m == nil {
		return -1
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// completionOf is the completion event (tool_called or tool_error) of id.
func completionOf(t *testing.T, events []store.Event, id string) store.Event {
	t.Helper()
	var found []store.Event
	for _, e := range events {
		if (e.Type == store.EventToolCalled || e.Type == store.EventToolError) && dataString(e, "tool_use_id") == id {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("completion events of %s = %d, want exactly 1 (events: %v)", id, len(found), events)
	}
	return found[0]
}

// reportOf decodes a codemode completion's report through the JSON names the
// event carries — scripthost.Limits decodes its own wire names, so a key the
// report misnamed would decode to zero here rather than pass.
func reportOf(t *testing.T, e store.Event) CodemodeReport {
	t.Helper()
	raw, ok := e.Data["codemode"]
	if !ok {
		t.Fatalf("%s of %s carries no codemode report: %v", e.Type, dataString(e, "tool_use_id"), e.Data)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var rep CodemodeReport
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("the codemode report %s does not decode: %v", b, err)
	}
	return rep
}

func echoGenTool() GenerationTool {
	return GenerationTool{Name: "echo", Description: "echo back", InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, in json.RawMessage) (string, error) { return string(in), nil }}
}

// startedInput is the decoded input of a nested call's tool_started event.
func startedInput(t *testing.T, events []store.Event, id string) map[string]any {
	t.Helper()
	for _, e := range events {
		if e.Type == store.EventToolStarted && dataString(e, "tool_use_id") == id {
			var in map[string]any
			if err := json.Unmarshal([]byte(dataString(e, "input")), &in); err != nil {
				t.Fatalf("the input of %s is not JSON: %v", id, err)
			}
			return in
		}
	}
	t.Fatalf("no tool_started for %s", id)
	return nil
}

// TestTwoProgramsInOneStepAttributeEachNestedCallToItsOwnProgram is the
// grouping key: two programs of one step, each nested call named by the
// program that made it — read from the call's own input, never from where
// the event sits in the stream — and numbered in program order. Frame 2 of
// each program is a run-state request, which spends a frame and emits no
// event, so a server numbering calls from its own counter would name the
// second echo 2 where its program issued it as 3.
func TestTwoProgramsInOneStepAttributeEachNestedCallToItsOwnProgram(t *testing.T) {
	t.Parallel()
	r := newInspectRig(t, inspectSetup{tools: []GenerationTool{echoGenTool()}, want: scripthost.Limits{Memory: 1 << 30}})
	source := func(tag string) string {
		return fmt.Sprintf(`
call("echo", {p: %[1]q, frame: 1});
try { call.load("k") } catch (e) {}
call("echo", {p: %[1]q, frame: 3});
call.parallel([{name: "echo", input: {p: %[1]q, frame: 4}}, {name: "echo", input: {p: %[1]q, frame: 5}}]);
true`, tag)
	}
	r.run(t, context.Background(), program{"toolu_A", source("A")}, program{"toolu_B", source("B")})
	events := r.snapshot()

	prefixes := map[string]string{}
	seen := map[string]int{}
	for _, e := range events {
		id := dataString(e, "tool_use_id")
		frame := nestedFrame(e)
		if frame < 0 {
			if id != "toolu_A" && id != "toolu_B" {
				t.Fatalf("an event names neither a program nor a nested call: %+v", e)
			}
			if _, ok := e.Data["parent_tool_use_id"]; ok {
				t.Errorf("the top-level call %s carries a parent key: %v — a reader groups by its presence", id, e.Data)
			}
			if _, ok := e.Data["batch"]; ok {
				t.Errorf("the top-level call %s carries a batch key: %v", id, e.Data)
			}
			continue
		}
		in := startedInput(t, events, id)
		tag, _ := in["p"].(string)
		if got, want := dataString(e, "parent_tool_use_id"), "toolu_"+tag; got != want {
			t.Errorf("%s %s (program %q) names parent %q, want %q", e.Type, id, tag, got, want)
		}
		if want, _ := in["frame"].(float64); frame != int64(want) {
			t.Errorf("%s %s carries frame %d where its program issued it as call %v: the id is not program order", e.Type, id, frame, want)
		}
		prefix := id[:strings.LastIndex(id, "_")]
		if prev, ok := prefixes[tag]; ok && prev != prefix {
			t.Errorf("program %q minted two id prefixes, %s and %s", tag, prev, prefix)
		}
		prefixes[tag] = prefix
		if e.Type == store.EventToolStarted {
			seen[tag]++
		}
	}
	if prefixes["A"] == "" || prefixes["A"] == prefixes["B"] {
		t.Errorf("the two programs' id prefixes = %v, want two distinct ones", prefixes)
	}
	if seen["A"] != 4 || seen["B"] != 4 {
		t.Errorf("nested starts per program = %v, want 4 each", seen)
	}
}

// TestEveryRefusedNestedCallNamesItsProgram walks each class of refusal a
// program's call can meet. A refused call is still a call the program made:
// its tool_error carries its own program-order id and its program's id.
func TestEveryRefusedNestedCallNamesItsProgram(t *testing.T) {
	t.Parallel()
	blocking := hooks.NewRunner()
	blocking.Register(hooks.PreToolUse, func(_ context.Context, hc hooks.Context) (hooks.Decision, error) {
		if hc.ToolName == "blocked" {
			return hooks.Decision{Action: hooks.ActionBlock, Reason: "not here"}, nil
		}
		return hooks.Decision{}, nil
	})
	scope := &tool.CommandScope{}
	scope.Apply("audit", []string{ScriptHostToolName, "echo"})
	other := echoGenTool()
	other.Name = "other"
	blocked := echoGenTool()
	blocked.Name = "blocked"
	tools := []GenerationTool{echoGenTool(), other, blocked}

	cases := []struct {
		name   string
		ctx    context.Context
		runner *hooks.Runner
		want   scripthost.Limits
		source string
		tool   string
		frames []int64
		error  string
	}{
		{name: "withheld", source: `try { call("ask_user", {question: "q"}) } catch (e) {}`, tool: "ask_user", frames: []int64{1}, error: "pauses the run"},
		{name: "unknown", source: `try { call("nope", {}) } catch (e) {}`, tool: "nope", frames: []int64{1}, error: "unknown tool: nope"},
		{name: "hook-blocked", runner: blocking, source: `try { call("blocked", {}) } catch (e) {}`, tool: "blocked", frames: []int64{1}, error: "blocked by hook: not here"},
		{
			name:   "narrowing",
			ctx:    tool.WithExecutableToolFilter(context.Background(), func(name string) bool { return name != "other" }),
			source: `try { call("other", {}) } catch (e) {}`, tool: "other", frames: []int64{1}, error: "not available to this caller",
		},
		{
			name:   "command-scope",
			ctx:    tool.WithCommandScope(context.Background(), scope),
			source: `try { call("other", {}) } catch (e) {}`, tool: "other", frames: []int64{1}, error: "not in allowed-tools",
		},
		{
			name:   "budget",
			want:   scripthost.Limits{ToolCalls: 1},
			source: `call.parallel([{name: "echo", input: {}}, {name: "other", input: {}}, {name: "other", input: {}}])`,
			tool:   "other", frames: []int64{2, 3}, error: "spent its budget",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := c.want
			want.Memory = 1 << 30
			r := newInspectRig(t, inspectSetup{tools: tools, want: want, runner: c.runner})
			ctx := c.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			r.run(t, ctx, program{"toolu_outer", c.source})
			events := r.snapshot()
			var frames []int64
			for _, e := range events {
				if dataString(e, "tool") != c.tool {
					continue
				}
				if e.Type != store.EventToolError {
					t.Errorf("the refused call emitted %s: %v", e.Type, e.Data)
					continue
				}
				frames = append(frames, nestedFrame(e))
				if got := dataString(e, "parent_tool_use_id"); got != "toolu_outer" {
					t.Errorf("the refusal names parent %q, want toolu_outer: %v", got, e.Data)
				}
				if !strings.Contains(dataString(e, "error"), c.error) {
					t.Errorf("the refusal reads %q, want it to say %q", dataString(e, "error"), c.error)
				}
			}
			slices.Sort(frames)
			if !slices.Equal(frames, c.frames) {
				t.Errorf("refused %s calls carry frames %v, want %v (events: %v)", c.tool, frames, c.frames, events)
			}
		})
	}
}

// TestABudgetRefusalKeepsItsBatchSlotAndTheReportCountsIt holds the budget's
// three facts on one program: the parent-side refusals are events with their
// batch slot, the report counts them, and a call the child refused itself
// after latching reaches nobody — it never crossed the channel.
func TestABudgetRefusalKeepsItsBatchSlotAndTheReportCountsIt(t *testing.T) {
	t.Parallel()
	r := newInspectRig(t, inspectSetup{tools: []GenerationTool{echoGenTool()}, want: scripthost.Limits{Memory: 1 << 30, ToolCalls: 2}})
	r.run(t, context.Background(), program{"toolu_budget", `
call("echo", {});
const r = call.parallel([{name: "echo", input: {}}, {name: "echo", input: {}}, {name: "echo", input: {}}]);
let latched = false;
try { call("echo", {}) } catch (e) { latched = true }
[r.map(x => x.status), latched]`})
	events := r.snapshot()
	out := completionOf(t, events, "toolu_budget")
	if got := dataString(out, "output"); got != `[["ok","budget","budget"],true]` {
		t.Fatalf("the program saw %s, want the batch's second and third entries refused and the next call latched", got)
	}
	slots := map[int64]map[string]any{}
	for _, e := range events {
		if f := nestedFrame(e); f >= 0 && e.Type != store.EventToolStarted {
			b, _ := e.Data["batch"].(map[string]any)
			slots[f] = b
		}
	}
	want := map[int64]map[string]any{
		1: nil,
		2: {"first": int64(2), "size": 3, "index": 0},
		3: {"first": int64(2), "size": 3, "index": 1},
		4: {"first": int64(2), "size": 3, "index": 2},
	}
	if fmt.Sprint(slots) != fmt.Sprint(want) {
		t.Errorf("nested completions by frame = %v, want %v — and no event for the latched fifth call", slots, want)
	}
	rep := reportOf(t, out)
	if rep.Calls != 2 || rep.BudgetRefused != 2 || rep.StateCalls != 0 {
		t.Errorf("report counts calls=%d budget_refused=%d state_calls=%d, want 2, 2, 0", rep.Calls, rep.BudgetRefused, rep.StateCalls)
	}
}

// TestABatchSlotFollowsTheCallNotTheOrderItFinished forces a two-entry batch
// to finish in each order and reads each completion's slot against the slot
// its own input declares.
func TestABatchSlotFollowsTheCallNotTheOrderItFinished(t *testing.T) {
	t.Parallel()
	for _, waiter := range []int{0, 1} {
		t.Run(fmt.Sprintf("entry-%d-finishes-last", waiter), func(t *testing.T) {
			t.Parallel()
			released := make(chan struct{})
			var once sync.Once
			slot := GenerationTool{Name: "slot", Description: "slot", InputSchema: json.RawMessage(`{"type":"object"}`), concurrentSafe: true,
				Execute: func(_ context.Context, in json.RawMessage) (string, error) {
					var args struct{ Index int }
					if err := json.Unmarshal(in, &args); err != nil {
						return "", err
					}
					if args.Index == waiter {
						select {
						case <-released:
						case <-time.After(10 * time.Second):
							return "", errors.New("the other entry never finished: the batch did not run concurrently")
						}
					}
					return fmt.Sprintf("done-%d", args.Index), nil
				}}
			var order []string
			r := newInspectRig(t, inspectSetup{
				tools:  []GenerationTool{slot},
				want:   scripthost.Limits{Memory: 1 << 30, ConcurrentToolCalls: 2},
				runner: NewDefaultLifecycleHooks(EventHooks{}),
				onEvent: func(e store.Event) {
					if e.Type == store.EventToolCalled && dataString(e, "tool") == "slot" {
						order = append(order, dataString(e, "output"))
						if dataString(e, "output") == fmt.Sprintf("done-%d", 1-waiter) {
							once.Do(func() { close(released) })
						}
					}
				},
			})
			r.run(t, context.Background(), program{"toolu_batch", `call.parallel([{name: "slot", input: {index: 0}}, {name: "slot", input: {index: 1}}]).map(x => x.text)`})
			events := r.snapshot()
			if want := []string{fmt.Sprintf("done-%d", 1-waiter), fmt.Sprintf("done-%d", waiter)}; !slices.Equal(order, want) {
				t.Fatalf("completions arrived as %v, want %v: the order under test was not forced", order, want)
			}
			for _, e := range events {
				if nestedFrame(e) < 0 {
					continue
				}
				in := startedInput(t, events, dataString(e, "tool_use_id"))
				b, _ := e.Data["batch"].(map[string]any)
				if b == nil || fmt.Sprint(b["index"]) != fmt.Sprint(in["index"]) || b["size"] != 2 || b["first"] != int64(1) {
					t.Errorf("%s of the entry declaring index %v carries batch %v", e.Type, in["index"], b)
				}
				if got := nestedFrame(e); fmt.Sprint(got-1) != fmt.Sprint(in["index"]) {
					t.Errorf("the entry declaring index %v carries frame %d", in["index"], got)
				}
			}
		})
	}
}

// TestTheReportStatesTheBoundsTheProgramRanUnder reads the report against
// scripthost's own Clamp, on a ceiling and on a floor, and checks the start
// event carries none of it.
func TestTheReportStatesTheBoundsTheProgramRanUnder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		want     scripthost.Limits
		source   string
		refusal  string
		adjusted []CodemodeLimitAdjustment
		calls    int64
	}{
		{
			name:   "ceiling",
			want:   scripthost.Limits{Memory: 1 << 30, ToolCalls: 99999, ConcurrentToolCalls: 99},
			source: `call("echo", {}); 1`,
			adjusted: []CodemodeLimitAdjustment{
				{Field: "concurrent_tool_calls", Requested: 99, Applied: scripthost.MaxParallelBatch},
				{Field: "tool_calls", Requested: 99999, Applied: scripthost.Ceiling().ToolCalls},
			},
			calls: 1,
		},
		{
			// Refused before any child exists, so a floor-raised memory bound
			// costs no process on the -race leg.
			name:     "floor",
			want:     scripthost.Limits{Memory: 1 << 20, ScriptBytes: 8},
			source:   `1+1+1+1+1`,
			refusal:  scripthost.RefusalScriptBytes,
			adjusted: []CodemodeLimitAdjustment{{Field: "memory_bytes", Requested: 1 << 20, Applied: scripthost.MemoryFloor().Memory}},
		},
		{
			name:   "nothing declared moves",
			want:   scripthost.Limits{Memory: 1 << 30},
			source: `2`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newInspectRig(t, inspectSetup{tools: []GenerationTool{echoGenTool()}, want: c.want})
			r.run(t, context.Background(), program{"toolu_bounds", c.source})
			events := r.snapshot()
			for _, e := range events {
				if e.Type == store.EventToolStarted {
					if _, ok := e.Data["codemode"]; ok {
						t.Errorf("tool_started of %s carries the report: it belongs to the completion", dataString(e, "tool_use_id"))
					}
				}
			}
			out := completionOf(t, events, "toolu_bounds")
			rep := reportOf(t, out)
			applied, _ := c.want.Clamp()
			if rep.Limits != applied {
				t.Errorf("report limits = %+v, want the clamped %+v", rep.Limits, applied)
			}
			if !slices.Equal(rep.Adjusted, c.adjusted) {
				t.Errorf("report adjusted = %+v, want %+v", rep.Adjusted, c.adjusted)
			}
			if _, present := out.Data["codemode"].(map[string]any)["adjusted"]; present != (len(c.adjusted) > 0) {
				t.Errorf("the adjusted key is present=%v with %d adjustments: it is omitted exactly when nothing moved", present, len(c.adjusted))
			}
			if ran := c.refusal == ""; (rep.Confinement != nil) != ran || (ran && rep.Confinement.CPUSeconds == 0) {
				t.Errorf("report confinement = %+v: want the child's own account exactly when a child ran", rep.Confinement)
			}
			if rep.Refusal != c.refusal || rep.Calls != c.calls || rep.SourceBytes != len(c.source) {
				t.Errorf("report refusal=%q calls=%d source_bytes=%d, want %q, %d, %d", rep.Refusal, rep.Calls, rep.SourceBytes, c.refusal, c.calls, len(c.source))
			}
		})
	}
}

// TestTheReportCarriesTheRefusalCodeTheChildProduced drives a real child
// into the refusals a program reaches cheaply, and checks the completion is
// the tool_error that carries the code.
func TestTheReportCarriesTheRefusalCodeTheChildProduced(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code   string
		want   scripthost.Limits
		source string
	}{
		{scripthost.RefusalScriptError, scripthost.Limits{}, `throw new Error("nope")`},
		{scripthost.RefusalScriptBytes, scripthost.Limits{ScriptBytes: 4}, `1+1+1`},
		{scripthost.RefusalResultBytes, scripthost.Limits{ResultBytes: 16}, `"x".repeat(64)`},
		{scripthost.RefusalWallClock, scripthost.Limits{Wall: 300 * time.Millisecond}, `while (true) {}`},
		{scripthost.RefusalCallStack, scripthost.Limits{}, `function f() { return f() } f()`},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			t.Parallel()
			want := c.want
			want.Memory = 1 << 30
			r := newInspectRig(t, inspectSetup{want: want})
			r.run(t, context.Background(), program{"toolu_refused", c.source})
			out := completionOf(t, r.snapshot(), "toolu_refused")
			if out.Type != store.EventToolError {
				t.Fatalf("a refused program completed as %s", out.Type)
			}
			if got := reportOf(t, out).Refusal; got != c.code {
				t.Errorf("report refusal = %q, want %q (error: %s)", got, c.code, dataString(out, "error"))
			}
		})
	}
}

// TestTheReportCarriesEveryRefusalCode closes the codes a real child cannot
// be steered into from a test (an unconfinable kernel, a child that dies
// without naming a bound, the kernel's own CPU and memory kills) with a
// child that writes the same document a real child writes — the verdict
// travels by that document, so the report reads it the same way.
func TestTheReportCarriesEveryRefusalCode(t *testing.T) {
	t.Parallel()
	for _, code := range []string{
		scripthost.RefusalUnavailable, scripthost.RefusalScriptBytes, scripthost.RefusalResultBytes,
		scripthost.RefusalWallClock, scripthost.RefusalCPUTime, scripthost.RefusalMemory,
		scripthost.RefusalHostDied, scripthost.RefusalScriptError, scripthost.RefusalCallStack,
	} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`{"refusal":%q,"message":"stub verdict"}`, code)
			host := scripthost.Host{Binary: writeStubChild(t, "#!/bin/sh\n/bin/cat >/dev/null\nprintf '%s' '"+doc+"'\n")}
			r := newInspectRig(t, inspectSetup{host: &host, want: scripthost.Limits{Memory: 1 << 30}})
			r.run(t, context.Background(), program{"toolu_code", `1`})
			out := completionOf(t, r.snapshot(), "toolu_code")
			if out.Type != store.EventToolError || reportOf(t, out).Refusal != code {
				t.Errorf("completion %s carries report %+v, want refusal %q (error: %s)", out.Type, reportOf(t, out), code, dataString(out, "error"))
			}
		})
	}
}

// writeStubChild writes an executable script without this process ever
// holding a write descriptor on it. A test that forks in parallel inherits
// every open descriptor until its exec, and a file some process holds open
// for writing refuses exec with ETXTBSY: os.WriteFile here failed the spawn
// as "text file busy" once in a full-package run. A shell writes the file
// and exits instead, so no writer outlives it.
func writeStubChild(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "child.sh")
	out, err := exec.Command("/bin/sh", "-c", `printf '%s' "$1" > "$2" && chmod 700 "$2"`, "sh", script, bin).CombinedOutput()
	if err != nil {
		t.Fatalf("writing the stub child %s: %v: %s", bin, err, out)
	}
	return bin
}

// TestTheReportCountsStateCalls: run-state requests spend the call budget and
// emit no event of their own, so the report is where they are counted.
func TestTheReportCountsStateCalls(t *testing.T) {
	t.Parallel()
	r := newInspectRig(t, inspectSetup{tools: []GenerationTool{echoGenTool()}, want: scripthost.Limits{Memory: 1 << 30}, state: testCodemodeState{}})
	r.run(t, context.Background(), program{"toolu_state", `call.store("k", 1); call.load("k"); call("echo", {}); true`})
	rep := reportOf(t, completionOf(t, r.snapshot(), "toolu_state"))
	if rep.Calls != 3 || rep.StateCalls != 2 || rep.BudgetRefused != 0 {
		t.Errorf("report calls=%d state_calls=%d budget_refused=%d, want 3, 2, 0", rep.Calls, rep.StateCalls, rep.BudgetRefused)
	}
}

// TestAProgramsSecretIsRedactedInTheStoredEvents: the program's source is
// the call's stored input, and it crosses the same redaction pass every
// tool input does — the source and a nested call that carries the value.
func TestAProgramsSecretIsRedactedInTheStoredEvents(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-CODEMODE0123456789abcdefghijkl"
	guard := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: secret}}, secretguard.DefaultConfig())
	r := newInspectRig(t, inspectSetup{tools: []GenerationTool{echoGenTool()}, want: scripthost.Limits{Memory: 1 << 30}, guard: guard})
	r.run(t, context.Background(), program{"toolu_secret", `const k = "` + secret + `"; call("echo", {k}); k.length`})
	events := r.snapshot()
	var programInput string
	for _, e := range events {
		b, err := json.Marshal(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), secret) {
			t.Errorf("%s of %s stores the secret in clear: %s", e.Type, dataString(e, "tool_use_id"), b)
		}
		if e.Type == store.EventToolStarted && dataString(e, "tool_use_id") == "toolu_secret" {
			programInput = dataString(e, "input")
		}
	}
	if !strings.Contains(programInput, "const k =") || strings.Contains(programInput, secret) {
		t.Errorf("the stored program source = %q, want the program with its secret placeholdered", programInput)
	}
	if out := completionOf(t, events, "toolu_secret"); out.Type != store.EventToolCalled || dataString(out, "output") != strconv.Itoa(len(secret)) {
		t.Errorf("the program completed as %s with %q, want the secret's length", out.Type, dataString(out, "output"))
	}
}

// TestRelayCarriesCodemodeLineageAndReport runs a real program on the
// RUNNER side of a sandboxed node, through the real relay and multiplexer,
// and reads what the launcher's store kept: the parent, the batch slot and
// the report all cross.
func TestRelayCarriesCodemodeLineageAndReport(t *testing.T) {
	t.Parallel()
	b, load := relayHost(t, "run-relay-codemode")
	var runErr error
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		opts := GenerationOptions{Tools: []GenerationTool{echoGenTool()}}
		applyHooks("campaign", 0, relay, &opts)
		want := scripthost.Limits{Memory: 1 << 30, ToolCalls: 99999}
		if err := installCodemodeOn(&opts, childHost(), false, &want, nil); err != nil {
			runErr = err
			return
		}
		in, _ := json.Marshal(map[string]string{"source": `call("echo", {}); call.parallel([{name: "echo", input: {}}, {name: "echo", input: {}}]).length`})
		_, runErr = executeToolsDirect(context.Background(), []toolUseBlock{{ID: "toolu_sandboxed", Name: ScriptHostToolName, PartialJSON: string(in)}},
			buildToolMap(opts.Tools), opts.OnToolStarted, opts.OnToolCall, nil, nil, nil, opts.toolObserversAudited)
	})
	if runErr != nil || len(errs) != 0 {
		t.Fatalf("runner side: %v, relay: %v", runErr, errs)
	}
	evts := load()
	var nested int
	for _, e := range evts {
		if e.Type != store.EventToolStarted && e.Type != store.EventToolCalled {
			continue
		}
		frame := nestedFrame(*e)
		if frame < 0 {
			continue
		}
		nested++
		if dataString(*e, "parent_tool_use_id") != "toolu_sandboxed" {
			t.Errorf("relayed %s of frame %d lost its parent: %v", e.Type, frame, e.Data)
		}
		b, _ := e.Data["batch"].(map[string]any)
		switch frame {
		case 1:
			if b != nil {
				t.Errorf("the single call of frame 1 carries a batch: %v", b)
			}
		default:
			if b == nil || b["first"] != float64(2) || b["size"] != float64(2) || b["index"] != float64(frame-2) {
				t.Errorf("relayed %s of frame %d carries batch %v", e.Type, frame, b)
			}
		}
	}
	if nested != 6 {
		t.Errorf("relayed nested events = %d, want 3 starts and 3 completions", nested)
	}
	var outer *store.Event
	for _, e := range eventsOfType(evts, store.EventToolCalled) {
		if dataString(*e, "tool_use_id") == "toolu_sandboxed" {
			outer = e
		}
	}
	if outer == nil {
		t.Fatal("the program's completion did not cross")
	}
	rep := reportOf(t, *outer)
	applied, _ := scripthost.Limits{Memory: 1 << 30, ToolCalls: 99999}.Clamp()
	if rep.Limits != applied || rep.Calls != 3 || len(rep.Adjusted) != 1 || rep.Adjusted[0].Field != "tool_calls" {
		t.Errorf("relayed report = %+v, want the clamped limits, 3 calls and the tool_calls adjustment", rep)
	}
}

// TestCodemodeReportNamesEveryBound: the report reads the bounds by their
// wire names off the struct tags, so a bound added to scripthost.Limits
// reaches the event only if every field is one the read understands.
func TestCodemodeReportNamesEveryBound(t *testing.T) {
	t.Parallel()
	got := limitsByWireName(scripthost.Ceiling())
	raw, err := json.Marshal(scripthost.Ceiling())
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]int64
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("a bound is not an integer on the wire, which the report cannot carry: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(wire) {
		t.Errorf("the report names %v, the wire carries %v", got, wire)
	}
}

// TestTheReportCarriesWhatTheChildEnforced: `limits` is what the host asked
// for, and the child's own account of what its kernel accepted rides beside
// it — a deployment's stricter rlimit or a whole-second CPU rounding shows
// there and nowhere else. The child here writes the document a real child
// writes, with a confinement no real kernel run would make deterministic.
func TestTheReportCarriesWhatTheChildEnforced(t *testing.T) {
	t.Parallel()
	doc := `{"value":"1","confinement":{"landlock_abi":4,"sockets_denied":true,"all_threads":true,"memory_bytes":1342177280,"cpu_seconds":1,"memory_base_bytes":90177536}}`
	host := scripthost.Host{Binary: writeStubChild(t, "#!/bin/sh\n/bin/cat >/dev/null\nprintf '%s' '"+doc+"'\n")}
	r := newInspectRig(t, inspectSetup{host: &host, want: scripthost.Limits{Memory: 1 << 30}})
	r.run(t, context.Background(), program{"toolu_enforced", `1`})
	out := completionOf(t, r.snapshot(), "toolu_enforced")
	rep := reportOf(t, out)
	want := scripthost.Applied{LandlockABI: 4, SocketsDenied: true, AllThreads: true, MemoryBytes: 1342177280, CPUSeconds: 1, MemoryBaseBytes: 90177536}
	if rep.Confinement == nil || *rep.Confinement != want {
		t.Fatalf("report confinement = %+v, want the child's %+v", rep.Confinement, want)
	}
	if rep.Limits.CPUSeconds() == rep.Confinement.CPUSeconds {
		t.Fatalf("the fixture cannot tell the two apart: requested %ds, enforced %ds", rep.Limits.CPUSeconds(), rep.Confinement.CPUSeconds)
	}
}

// TestTheReportStatesTheWallTheNodeDeadlineLeft: a node deadline nearer than
// the wall bound is the program's real wall, and the report says so; a
// deadline further out, or none, adds nothing.
func TestTheReportStatesTheWallTheNodeDeadlineLeft(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		deadline time.Duration
		cut      bool
	}{
		{"nearer than the wall", 20 * time.Second, true},
		{"further than the wall", 10 * time.Minute, false},
		{"none", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if c.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, c.deadline)
				defer cancel()
			}
			r := newInspectRig(t, inspectSetup{want: scripthost.Limits{Memory: 1 << 30}})
			r.run(t, ctx, program{"toolu_wall", `1`})
			out := completionOf(t, r.snapshot(), "toolu_wall")
			rep := reportOf(t, out)
			if _, present := out.Data["codemode"].(map[string]any)["effective_wall_ns"]; present != c.cut {
				t.Fatalf("effective_wall_ns present=%v, want %v (report %+v)", present, c.cut, rep)
			}
			if c.cut && (rep.EffectiveWall <= 0 || rep.EffectiveWall > c.deadline || rep.EffectiveWall >= rep.Limits.Wall) {
				t.Errorf("effective wall = %s, want within the node's %s and under the %s bound", rep.EffectiveWall, c.deadline, rep.Limits.Wall)
			}
		})
	}
}

// refuseOverTheWire sends each call envelope through the REAL multiplexer to
// a launcher whose policy denies `mcp__github__delete_*`, and checks each is
// refused and never runs on the host.
func refuseOverTheWire(t *testing.T, hooks EventHooks, calls ...delegate.Envelope) {
	t.Helper()
	pol, err := permission.NewPolicy(permission.ModeDeny, []string{"*"}, nil, []string{"mcp__github__delete_*"})
	if err != nil {
		t.Fatal(err)
	}
	task, ran := identityTask(pol)
	handler, _ := NewClawBackend(NewRegistry(), hooks, RetryPolicy{}).multiplexerHandler(context.Background(), task, sessionSeed{}, newCrossingApprovalState())
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()
	mux := delegate.NewMultiplexer(runnerStdoutR, runnerStdinW, handler)
	done := make(chan error, 1)
	go func() {
		_, err := mux.Run(context.Background())
		done <- err
	}()
	writer := delegate.NewEnvelopeWriter(runnerStdoutW)
	replies := delegate.NewEnvelopeReader(runnerStdinR)
	for _, env := range calls {
		if err := writer.Write(env); err != nil {
			t.Fatal(err)
		}
		reply, err := replies.Read()
		if err != nil || reply.Type != delegate.EnvelopeToolResult || !strings.Contains(string(reply.Data), "denied") {
			t.Fatalf("reply = %+v (%s), err %v: want the refusal", reply, reply.Data, err)
		}
	}
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(resEnv); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("multiplexer: %v", err)
	}
	if *ran {
		t.Fatal("the denied tool ran on the host")
	}
}

func mustEnvelope(t *testing.T) func(delegate.Envelope, error) delegate.Envelope {
	return func(env delegate.Envelope, err error) delegate.Envelope {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
}

// TestALauncherRefusalNamesTheForwardedCall: a program's nested call the
// sandboxed runner forwards and the launcher refuses is recorded by the
// launcher under the id and parent the runner sent — marked as the
// launcher's own, since both are the runner's claim — and an older runner's
// call, which sends neither, is still recorded, id-less. Driven through the
// real multiplexer, so the identity crosses the wire as JSON.
func TestALauncherRefusalNamesTheForwardedCall(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []store.Event
	emitter := codemodeEventEmitter(func(e store.Event) { mu.Lock(); events = append(events, e); mu.Unlock() })
	env := mustEnvelope(t)
	refuseOverTheWire(t, NewStoreEventHooks(context.Background(), emitter, "run", iterlog.Nop(), nil, nil),
		env(delegate.NewToolCallEnvelopeFor("c1", delegate.ToolCallData{
			Name: "mcp_github_delete_repo", Input: json.RawMessage(`{}`),
			ToolUseID: "codemode_ABC_3", ParentToolUseID: "toolu_prog",
		})),
		env(delegate.NewToolCallEnvelope("c2", "mcp_github_delete_repo", json.RawMessage(`{}`))))
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Type != store.EventToolError || events[1].Type != store.EventToolError {
		t.Fatalf("launcher events = %+v, want two tool_error", events)
	}
	if got := events[0].Data; got["tool_use_id"] != "codemode_ABC_3" || got["parent_tool_use_id"] != "toolu_prog" || got["emitted_by"] != ToolEventEmittedByLauncher {
		t.Errorf("the refusal of the identified call = %v, want its claimed id and parent, marked as the launcher's", got)
	}
	if got := events[1].Data; got["tool_use_id"] != nil || got["parent_tool_use_id"] != nil || got["tool"] != "mcp_github_delete_repo" || got["emitted_by"] != ToolEventEmittedByLauncher {
		t.Errorf("an older runner's refusal = %v, want it recorded with no id and no parent, marked", got)
	}
}

// TestARelayedEventCannotClaimTheLaunchersMark: the mark is the launcher's
// alone. A runner that writes `emitted_by` into a relayed tool event gets an
// event without it — the relayed path decodes the wire form field by field.
func TestARelayedEventCannotClaimTheLaunchersMark(t *testing.T) {
	t.Parallel()
	var events []store.Event
	hooks := NewStoreEventHooks(context.Background(), codemodeEventEmitter(func(e store.Event) { events = append(events, e) }), "run", iterlog.Nop(), nil, nil)
	for _, typ := range []store.EventType{store.EventToolStarted, store.EventToolCalled} {
		handled, err := ApplyRelayedEvent(hooks, "n", string(typ), map[string]any{
			"tool": "mcp_github_delete_repo", "tool_use_id": "codemode_ABC_3", "parent_tool_use_id": "toolu_prog",
			"error": "forged", "emitted_by": ToolEventEmittedByLauncher,
		})
		if !handled || err != nil {
			t.Fatalf("%s: handled=%v err=%v", typ, handled, err)
		}
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	for _, e := range events {
		if _, forged := e.Data["emitted_by"]; forged {
			t.Errorf("the relayed %s carries the launcher's mark: %v", e.Type, e.Data)
		}
	}
}

// TestTheLauncherRefusalLogsNoInputByTheClaimedID: the run log line of the
// launcher's refusal does not print the input remembered under the id the
// runner claimed — that input may be another call's — and leaves it for the
// call's own event.
func TestTheLauncherRefusalLogsNoInputByTheClaimedID(t *testing.T) {
	t.Parallel()
	var logged syncBuffer
	logger := iterlog.New(iterlog.LevelInfo, &logged)
	hooks := NewStoreEventHooks(context.Background(), codemodeEventEmitter(func(store.Event) {}), "run", logger, nil, nil)
	const secretInput = `{"path":"ANOTHER-CALLS-INPUT"}`
	hooks.OnToolStarted("n", LLMToolStartedInfo{ToolName: "read_file", ToolUseID: "toolu_other", Input: json.RawMessage(secretInput)})
	refuseOverTheWire(t, hooks, mustEnvelope(t)(delegate.NewToolCallEnvelopeFor("c1", delegate.ToolCallData{
		Name: "mcp_github_delete_repo", Input: json.RawMessage(`{}`), ToolUseID: "toolu_other",
	})))
	if out := logged.String(); strings.Contains(out, "ANOTHER-CALLS-INPUT") || !strings.Contains(out, "launcher") {
		t.Fatalf("the launcher's refusal log line = %q: want it marked and without the input of the call whose id it claimed", out)
	}
	hooks.OnToolCall("n", LLMToolCallInfo{ToolName: "read_file", ToolUseID: "toolu_other", Error: errors.New("own failure")})
	if out := logged.String(); !strings.Contains(out, "ANOTHER-CALLS-INPUT") {
		t.Errorf("the call's own error lost its input to the launcher's refusal: %q", out)
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's writes and the test's
// reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTheReportKeepsWhatTheChildEnforcedWhenTheKernelKillsIt: a deployment
// whose own CPU limit is stricter than the program's kills the child before
// any verdict, and the report still says what the child enforced — it said
// so before its script ran. The wrapper is the deployment: `ulimit -t 2`
// around the real child.
func TestTheReportKeepsWhatTheChildEnforcedWhenTheKernelKillsIt(t *testing.T) {
	t.Parallel()
	host := childHost()
	host.Binary = writeStubChild(t, "#!/bin/sh\nulimit -t 2\nexec '"+os.Args[0]+"' \"$@\"\n")
	r := newInspectRig(t, inspectSetup{host: &host, want: scripthost.Limits{Memory: 1 << 30}})
	r.run(t, context.Background(), program{"toolu_killed", `while (true) {}`})
	out := completionOf(t, r.snapshot(), "toolu_killed")
	rep := reportOf(t, out)
	if out.Type != store.EventToolError || rep.Refusal == "" {
		t.Fatalf("the killed program completed as %s with refusal %q: %s", out.Type, rep.Refusal, dataString(out, "error"))
	}
	if rep.Confinement == nil || rep.Confinement.CPUSeconds != 2 {
		t.Fatalf("report confinement = %+v, want the 2 CPU seconds the child enforced (error: %s)", rep.Confinement, dataString(out, "error"))
	}
	if !strings.Contains(dataString(out, "error"), "against a 2s bound") {
		t.Errorf("the refusal does not name the bound that applied: %s", dataString(out, "error"))
	}
}
