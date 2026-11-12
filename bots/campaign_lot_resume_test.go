package bots

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Where a campaign lot starts (#3181). lot_room admits each lot the
// campaign's edges start. A resume of a campaign whose lot failed restarts
// at run_lot itself — the failed subbot node is the checkpoint, and a failed
// child is replaced by a fresh one — so lot_room is not evaluated again; the
// fresh child judges the same predicate where every lot starts, over the
// campaign clock run_lot passes it.

// childRoom is modernize's campaign_room gate as the compiled child holds it.
type childRoom struct {
	modernize *ir.Workflow
	node      *ir.ComputeNode
}

// modernizeRoomGate finds the child's gate: modernize's entry is a compute
// node whose `room` field routes to done when false and to the plan reader
// when true, so no node of a lot runs before it decides.
func modernizeRoomGate(t *testing.T) childRoom {
	t.Helper()
	modernize := compileBot(t, "modernize")
	if modernize == nil {
		t.Fatal("modernize did not compile")
	}
	gate, ok := modernize.Nodes[modernize.Entry].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("modernize's entry %q is %T, not the compute gate that judges the campaign's room before any node of the lot runs", modernize.Entry, modernize.Nodes[modernize.Entry])
	}
	toDone, toWork := false, false
	for _, e := range modernize.Edges {
		if e.From != gate.ID {
			continue
		}
		if e.Condition != "room" || e.Expression != nil {
			t.Fatalf("edge %s -> %s leaves the room gate on %q (expression %q), not on its `room` field", e.From, e.To, e.Condition, e.ExpressionSrc)
		}
		if e.Negated {
			if _, done := modernize.Nodes[e.To].(*ir.DoneNode); !done {
				t.Fatalf("with no room the child goes to %s (%T), not to done — a lot that cannot finish would still start", e.To, modernize.Nodes[e.To])
			}
			toDone = true
		} else {
			toWork = true
		}
	}
	if !toDone || !toWork {
		t.Fatalf("the room gate %s routes room to work %t, no room to done %t — want both", gate.ID, toWork, toDone)
	}
	return childRoom{modernize: modernize, node: gate}
}

// eval evaluates one field of the child's gate over the vars a launch
// gives it, defaults filled in as the engine fills them.
func (g childRoom) eval(t *testing.T, field string, vars map[string]any) any {
	t.Helper()
	var ast *expr.AST
	for _, ce := range g.node.Exprs {
		if ce.Key == field {
			ast = ce.AST
		}
	}
	if ast == nil {
		t.Fatalf("modernize's %s computes no %q", g.node.ID, field)
	}
	v, err := ast.Eval(&expr.Context{Vars: func(path []string) any {
		if len(path) != 1 {
			return nil
		}
		if v, ok := vars[path[0]]; ok {
			return v
		}
		if d := g.modernize.Vars[path[0]]; d != nil && d.HasDefault {
			return d.Default
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("evaluating modernize's %s.%s over %v: %v", g.node.ID, field, vars, err)
	}
	return v
}

// lotVars is what campaign's run_lot hands its child at elapsed of a wall:
// its `with:` mapping, each value one whole `{{run.*}}` or `{{vars.*}}`
// reference resolved as the engine resolves it then — a campaign var from
// set when the launch sets it, else from its default.
func lotVars(t *testing.T, campaign *ir.Workflow, lotID string, elapsed, wall time.Duration, set map[string]int64) map[string]any {
	t.Helper()
	lot := campaign.Nodes[lotID].(*ir.SubbotNode)
	out := map[string]any{}
	for _, m := range lot.With {
		ref := strings.TrimSpace(m.Raw)
		if !strings.HasPrefix(ref, "{{") || !strings.HasSuffix(ref, "}}") || strings.Count(ref, "{{") != 1 {
			continue
		}
		switch ref = strings.TrimSpace(ref[2 : len(ref)-2]); {
		case ref == "run.elapsed_seconds":
			out[m.Key] = elapsed.Seconds()
		case ref == "run.max_duration_seconds":
			out[m.Key] = wall.Seconds()
		case strings.HasPrefix(ref, "vars."):
			name := strings.TrimPrefix(ref, "vars.")
			if v, ok := set[name]; ok {
				out[m.Key] = v
			} else if d := campaign.Vars[name]; d != nil && d.HasDefault {
				out[m.Key] = d.Default
			}
		}
	}
	return out
}

// TestModernizeJudgesTheCampaignsRoomAsLotRoomDoes: the child's gate, over
// what run_lot passes it, says exactly what lot_room says over the
// campaign's own clock, at every point TestCampaignStartsALotOnlyWhereOneCanFinish
// probes — the resumed start is held to the bound the first one is — and its
// notice restates its verdict. That holds for lot_room_s at its default and
// set by the launch (the README's short-wall 2400, a 900 s lot): the escape
// hatch that admits a lot at the first start admits it at a resumed one. A
// direct launch, which passes no clock, is never held.
func TestModernizeJudgesTheCampaignsRoomAsLotRoomDoes(t *testing.T) {
	t.Parallel()
	g := campaignLotGate(t)
	child := modernizeRoomGate(t)
	lotID := ""
	for _, e := range g.campaign.Edges {
		if e.From == g.id && e.Condition == g.field && !e.Negated {
			lotID = e.To
		}
	}
	for _, set := range []map[string]int64{nil, {"lot_room_s": 2400}, {"lot_room_s": 900}} {
		g.set = set
		for _, wall := range []time.Duration{0, 48 * time.Hour, 30 * time.Hour, 20 * time.Hour, 30 * time.Minute} {
			for elapsed := time.Duration(0); elapsed <= max(wall, time.Hour); elapsed += 5 * time.Minute {
				parent, _ := g.eval(t, g.field, elapsed, wall, 1).(bool)
				vars := lotVars(t, g.campaign, lotID, elapsed, wall, set)
				got, ok := child.eval(t, "room", vars).(bool)
				if !ok {
					t.Fatalf("modernize's %s.room is not a bool over %v", child.node.ID, vars)
				}
				if got != parent {
					t.Fatalf("at %s of a %s wall, launch vars %v, lot_room says room=%t and the child, over what run_lot passes it (%v), says %t — a lot a resume starts is not held to the bound lot_room holds the first one to",
						elapsed, wall, set, parent, vars, got)
				}
				if notice, _ := child.eval(t, "notice", vars).(string); (notice == "room for a lot") != got {
					t.Fatalf("at %s of a %s wall, launch vars %v, the child's room is %t and its notice reads %q — the notice restates the verdict", elapsed, wall, set, got, notice)
				}
			}
		}
	}
	if room, _ := child.eval(t, "room", nil).(bool); !room {
		t.Fatal("modernize launched directly, with no campaign clock, takes no lot")
	}
}

// testClock is a budget clock a test advances by hand.
type testClock struct{ offset atomic.Int64 }

func (c *testClock) now() time.Time {
	return time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).Add(time.Duration(c.offset.Load()))
}

func (c *testClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// shapedOutput is a tool node's output with every field its schema declares
// at its zero value, then `set`.
func shapedOutput(t *testing.T, wf *ir.Workflow, n ir.Node, set map[string]any) map[string]any {
	t.Helper()
	tool, ok := n.(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is %T, not a tool node this stub answers", n.NodeID(), n)
	}
	out := map[string]any{}
	if s := wf.Schemas[tool.OutputSchema]; s != nil {
		for _, f := range s.Fields {
			switch f.Type {
			case ir.FieldTypeBool:
				out[f.Name] = false
			case ir.FieldTypeInt:
				out[f.Name] = int64(0)
			case ir.FieldTypeFloat:
				out[f.Name] = 0.0
			case ir.FieldTypeStringArray:
				out[f.Name] = []any{}
			case ir.FieldTypeJSON:
				out[f.Name] = []any{}
			default:
				out[f.Name] = ""
			}
		}
	}
	for k, v := range set {
		out[k] = v
	}
	return out
}

// nodeStub answers a workflow's tool nodes from `answer`, recording each
// node it runs; a node `answer` does not know fails the test.
type nodeStub struct {
	answer func(id string, input map[string]any) (map[string]any, error)
	mu     sync.Mutex
	ran    []string
}

func (s *nodeStub) Execute(_ context.Context, n ir.Node, input map[string]any) (map[string]any, error) {
	s.mu.Lock()
	s.ran = append(s.ran, n.NodeID())
	s.mu.Unlock()
	return s.answer(n.NodeID(), input)
}

func (s *nodeStub) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.ran {
		if r == id {
			n++
		}
	}
	return n
}

// TestCampaignResumedLotStartsOnlyWhereOneCanFinish drives the REAL campaign
// and the REAL modernize through the engine, their tool nodes stubbed and
// the budget clock advanced by hand. Under campaign's 48h wall, lot_room
// admits a lot at 27h (27h + lot_room_s 15h < 43.2h); the child fails 12h
// in, at its own wall, so the campaign fails at run_lot, resumable, at 39h.
// Resumed, run_lot starts a fresh child at 39h, where lot_room would refuse
// one (39h + 15h >= 43.2h): the child takes no lot — its plan reader never
// runs — and the campaign hands off, saying why (lot_room's refusal, or the
// loop's budget guard declining a lot as long as the last one). A resumed
// start that took the lot would run it to about 53h, past
// 90% of the wall, where observe and finalize are refused: no handoff.
func TestCampaignResumedLotStartsOnlyWhereOneCanFinish(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	campaign := compileBot(t, "campaign")
	modernize := compileBot(t, "modernize")
	if campaign == nil || modernize == nil {
		t.Fatal("campaign or modernize did not compile")
	}
	if wall := literalWall(t, "campaign", campaign.Budget); wall != 48*time.Hour {
		t.Fatalf("campaign's wall is %s: this scenario's figures are set for 48h — rework them", wall)
	}
	if room := intVarDefault(t, "campaign", campaign, "lot_room_s"); room != 54000 {
		t.Fatalf("campaign's lot_room_s is %d: this scenario's figures are set for 54000 — rework them", room)
	}

	clock := &testClock{}
	var lotStarts []time.Duration
	var lotsMu sync.Mutex
	children := atomic.Int32{}
	endedBy := ""
	parent := &nodeStub{}
	parent.answer = func(id string, input map[string]any) (map[string]any, error) {
		n := campaign.Nodes[id]
		switch id {
		case "preflight":
			clock.advance(27 * time.Hour) // the lots before this one
			return shapedOutput(t, campaign, n, map[string]any{"verify_path": "sh verify.sh", "campaign_base": "base", "ledger_path": "ledger.md"}), nil
		case "observe":
			return shapedOutput(t, campaign, n, map[string]any{"before": "base", "moved": true}), nil
		case "steward":
			return shapedOutput(t, campaign, n, nil), nil
		case "finalize":
			lotsMu.Lock()
			endedBy, _ = input["ended_by"].(string)
			lotsMu.Unlock()
			return shapedOutput(t, campaign, n, map[string]any{"handoff_path": ".modernize/campaign/handoff.md"}), nil
		}
		return nil, fmt.Errorf("the scenario does not reach %s", id)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		child := &nodeStub{}
		child.answer = func(id string, _ map[string]any) (map[string]any, error) {
			if id != "plan_read" {
				return nil, fmt.Errorf("the scenario's lot ends in its plan reader, not at %s", id)
			}
			lotsMu.Lock()
			lotStarts = append(lotStarts, time.Duration(clock.offset.Load()))
			lotsMu.Unlock()
			clock.advance(12 * time.Hour)
			return nil, errors.New("the lot ran to modernize's wall")
		}
		id := fmt.Sprintf("lot-child-%d", children.Add(1))
		eng := runtime.New(modernize, st, child, runtime.WithBudgetClock(clock.now))
		if err := eng.Run(ctx, id, req.Vars); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	}
	ctx := context.Background()
	const runID = "campaign-lot-resume"
	newEngine := func() *runtime.Engine {
		return runtime.New(campaign, st, parent, runtime.WithSubbotRunner(runner), runtime.WithBudgetClock(clock.now))
	}
	if err := newEngine().Run(ctx, runID, nil); err == nil {
		t.Fatal("the first attempt ended without the lot's failure")
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailedResumable || r.Checkpoint == nil || r.Checkpoint.NodeID != "run_lot" {
		t.Fatalf("after the lot's failure the campaign is %s at %v — the scenario needs it resumable at run_lot", r.Status, r.Checkpoint)
	}
	started := func() []time.Duration {
		lotsMu.Lock()
		defer lotsMu.Unlock()
		return slices.Clone(lotStarts)
	}
	if s := started(); len(s) != 1 || s[0] != 27*time.Hour {
		t.Fatalf("lots started at %v: lot_room's first admission at 27h must start the lot", s)
	}

	err = newEngine().Resume(ctx, runID, nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("the resumed campaign ended with %v, not parked on its handoff review — lots started at %v", err, started())
	}
	if s := started(); len(s) != 1 {
		t.Fatalf("lots started at %v: the resumed run_lot started one at 39h, where it cannot finish before 90%% of the 48h wall (39h + lot_room_s 15h >= 43.2h)", s)
	}
	if children.Load() != 2 {
		t.Fatalf("%d child runs: the resume must start a fresh child, which judges the room and takes no lot", children.Load())
	}
	if c, err := st.LoadRun(ctx, "lot-child-2"); err != nil || c.Status != store.RunStatusFinished {
		t.Fatalf("the resumed start's child is %v (%v): it ends finished, having taken no lot", c, err)
	}
	lotsMu.Lock()
	why := endedBy
	lotsMu.Unlock()
	if parent.count("finalize") != 1 || why == "" {
		t.Fatalf("finalize ran %d time(s), ended_by %q: the campaign hands off, saying why", parent.count("finalize"), why)
	}
}
