package cloudpublisher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// pliageDeclarationFixture is the platform's `pliage` feed declaration, its
// entries as production served them (read-only GET /api/admin/price-feeds,
// 2026-11-15).
const pliageDeclarationFixture = "testdata/pliage_declared_entries.json"

// TestReviGatewayCells_EveryEffortIsSentAsTheCellAsks reads the effort cells
// of the Revi gateway block (testdata/revi_gateway_routing.json, the block
// scripts/ops/revi-effort-cells.py applies) against the platform's `pliage`
// feed, and runs every celled node of the shipped bots under it:
//
//   - each `effort` has its reason in the fixture's `effort_rationale`, and
//     each reason names a celled node and opens on its level;
//   - each `effort` is a level the feed lists for the cell's model — the run
//     table the declaration folds to, read by the ladder every effort writer
//     reads (compatgw.AcceptedEfforts), answering from `feed:pliage`;
//   - the launch's fold hands the node the cell's effort, and the executor,
//     with that run table on the run's context, hands the backend exactly
//     that level and emits no reasoning_effort_adjusted; the node's output
//     says so (`_reasoning_effort_applied`, source `override`).
//
// Mutation that reddens it: write `"effort": "medium"` on any cell of the
// fixture — the feed lists no medium, and the route then sends `high` with
// an adjustment event.
func TestReviGatewayCells_EveryEffortIsSentAsTheCellAsks(t *testing.T) {
	fixture := readReviGatewayRouting(t)
	block := reviGatewayBlock(t)
	efforts := map[string]llmroute.Override{}
	for _, o := range block.Overrides {
		if o.Effort != "" {
			efforts[o.Selector] = o
		}
	}
	if len(efforts) == 0 {
		t.Fatal("the fixture's block pins no effort — the check judges nothing")
	}
	for sel, o := range efforts {
		why, ok := fixture.EffortRationale[sel]
		if !ok {
			t.Errorf("cell %q pins effort %q with no reason in effort_rationale", sel, o.Effort)
			continue
		}
		if !strings.HasPrefix(why, o.Effort+" ") {
			t.Errorf("effort_rationale[%q] = %q does not open on the cell's level %q", sel, why, o.Effort)
		}
	}
	for sel := range fixture.EffortRationale {
		if _, ok := efforts[sel]; !ok {
			t.Errorf("effort_rationale names %q, which pins no effort in the block", sel)
		}
	}

	table := pliageRunTable(t)
	get := func(name string) string {
		if name == compatgw.FeedTableEnv {
			return string(table)
		}
		return ""
	}
	for sel, o := range efforts {
		wire := modelroute.Parse(o.Model).Wire
		accepted, source := compatgw.AcceptedEfforts(wire, get, func(format string, args ...any) { t.Errorf(format, args...) })
		if source != compatgw.SourceFeed("pliage") {
			t.Errorf("cell %q: the efforts of %q come from %q, want the pliage feed", sel, wire, source)
		}
		if !slices.Contains(accepted, o.Effort) {
			t.Errorf("cell %q pins effort %q, which the pliage feed does not list for %q (it lists %v): the route would replace it", sel, o.Effort, wire, accepted)
		}
	}

	gatewayCellEnv(t)
	pol := reviGatewayPolicy(t, llmroute.SourceWebhook)
	ran := 0
	for _, bot := range reviBots {
		freezeCatalogBotDials(t, bot)
		wf := compileShippedBot(t, bot)
		view := reviGatewayView(t, bot, wf, pol)
		for sel, o := range efforts {
			cellBot, target, _ := strings.Cut(sel, "/")
			if cellBot != bot {
				continue
			}
			t.Run(sel, func(t *testing.T) {
				node, ok := wf.Nodes[target]
				if !ok {
					t.Fatalf("%s has no node %q", bot, target)
				}
				if got := view.Overrides.ForNode(node.NodeID(), node.NodeKind()).Effort; got != o.Effort {
					t.Fatalf("the launch's fold hands %q effort %q, want the cell's %q", target, got, o.Effort)
				}
				sent, adjusted, out := runUnderCells(t, wf, node, view.Overrides, table)
				if sent != o.Effort {
					t.Errorf("%q was dispatched at effort %q, want the cell's %q", target, sent, o.Effort)
				}
				if len(adjusted) != 0 {
					t.Errorf("%q drew reasoning_effort_adjusted: %+v", target, adjusted)
				}
				if out[model.OutputReasoningEffortApplied] != o.Effort || out[model.OutputReasoningEffortSource] != model.EffortSourceOverride {
					t.Errorf("%q's output says effort %v from %v, want %q from %q", target, out[model.OutputReasoningEffortApplied], out[model.OutputReasoningEffortSource], o.Effort, model.EffortSourceOverride)
				}
			})
			ran++
		}
	}
	if ran != len(efforts) {
		t.Errorf("ran %d celled node(s), want every one of the %d effort cells", ran, len(efforts))
	}
}

// pliageRunTable folds the platform's declaration the way the server folds
// it into a run's feed table, and answers the table's wire form.
func pliageRunTable(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(pliageDeclarationFixture)
	if err != nil {
		t.Fatal(err)
	}
	var decl struct {
		Feed    string          `json:"feed"`
		Type    string          `json:"type"`
		Entries json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &decl); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	table := pricefeed.Fold(pricefeed.FoldConfig{Now: now}, pricefeed.ScopeState{Config: &pricefeed.ConfigRecord{
		UpdatedAt: now,
		Feeds: []pricefeed.FeedConfig{{
			Name: decl.Feed, Type: decl.Type, Enabled: true,
			Interval: pricefeed.D(30 * 24 * time.Hour), Timeout: pricefeed.D(time.Second),
			Params: map[string]string{"entries": string(decl.Entries)},
		}},
	}})
	if table == nil {
		t.Fatal("the pliage declaration folded to nothing")
	}
	out, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gatewayCellEnv is a runner that can dispatch to the team gateway — a base
// URL and a key — and carries no env effort table of its own: the run's feed
// table is the one that answers.
func gatewayCellEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:8081/v1")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "test-key")
	for _, name := range []string{compatgw.CatalogModelsEnv, compatgw.ReasoningEffortsEnv, compatgw.CatalogProviderEnv, compatgw.FeedTableEnv, compatgw.ResolvedEnvName} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// effortRecorder is the claw backend of the run: it records the effort each
// call was handed and answers the node's schema with its zero values.
type effortRecorder struct {
	mu     sync.Mutex
	schema *ir.Schema
	sent   []string
}

func (r *effortRecorder) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	r.mu.Lock()
	r.sent = append(r.sent, task.ReasoningEffort)
	r.mu.Unlock()
	return delegate.Result{Output: zeroOutputFor(r.schema), BackendName: delegate.BackendClaw, Tokens: 1, EffectiveModel: task.Model}, nil
}

// zeroOutputFor is a value of every field the schema declares.
func zeroOutputFor(schema *ir.Schema) map[string]any {
	out := map[string]any{}
	if schema == nil {
		return out
	}
	for _, f := range schema.Fields {
		switch f.Type {
		case ir.FieldTypeString:
			out[f.Name] = ""
			if len(f.EnumValues) > 0 {
				out[f.Name] = f.EnumValues[0]
			}
		case ir.FieldTypeBool:
			out[f.Name] = false
		case ir.FieldTypeInt, ir.FieldTypeFloat:
			out[f.Name] = float64(0)
		case ir.FieldTypeStringArray:
			out[f.Name] = []any{}
		case ir.FieldTypeFile:
			out[f.Name] = "out.txt"
		default:
			out[f.Name] = map[string]any{}
		}
	}
	return out
}

// runUnderCells executes node through the executor with the launch's folded
// overrides and the run's feed table on its context, its claw backend the
// recorder; it answers the effort the backend was handed, the adjustment
// events, and the node's output.
func runUnderCells(t *testing.T, wf *ir.Workflow, node ir.Node, overrides model.ModelOverrides, table []byte) (string, []model.ReasoningEffortAdjustment, map[string]any) {
	t.Helper()
	var schemaName string
	var tools, caps []string
	switch n := node.(type) {
	case *ir.AgentNode:
		schemaName, tools, caps = n.OutputSchema, n.Tools, n.Capabilities
	case *ir.JudgeNode:
		schemaName, tools, caps = n.OutputSchema, n.Tools, n.Capabilities
	default:
		t.Fatalf("%q is a %T", node.NodeID(), node)
	}
	rec := &effortRecorder{schema: wf.Schemas[schemaName]}
	backends := delegate.NewRegistry()
	backends.Register(delegate.BackendClaw, rec)
	reg := tool.NewRegistry()
	for _, name := range append(slices.Clone(tools), "todo_write") {
		if err := reg.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(caps) > 0 {
		board, err := native.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = board.Close() })
		if err := tool.RegisterClawBoardTools(reg, &tool.BoardConfig{Store: board, Capabilities: boardops.AllCapabilities()}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var adjusted []model.ReasoningEffortAdjustment
	ws := t.TempDir()
	e := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(backends), model.WithDefaultBackend(delegate.BackendClaw),
		model.WithModelOverrides(overrides), model.WithToolRegistry(reg), model.WithWorkDir(ws),
		model.WithLogger(iterlog.Nop()),
		model.WithEventHooks(model.EventHooks{OnReasoningEffortAdjusted: func(_ string, info model.ReasoningEffortAdjustment) {
			mu.Lock()
			defer mu.Unlock()
			adjusted = append(adjusted, info)
		}}))
	e.SetVars(map[string]any{"workspace_dir": ws})
	ctx, cancel := context.WithTimeout(compatgw.OverlayFeedTable(context.Background(), table), time.Minute)
	defer cancel()
	out, err := e.Execute(ctx, node, map[string]any{})
	if err != nil {
		t.Fatalf("execute %q: %v", node.NodeID(), err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sent) != 1 {
		t.Fatalf("%q reached its backend %d time(s), want once: %v", node.NodeID(), len(rec.sent), rec.sent)
	}
	mu.Lock()
	defer mu.Unlock()
	return rec.sent[0], slices.Clone(adjusted), out
}

// The fixture names the files the procedure reads, so a renamed script is
// caught here rather than at the operator's terminal.
func TestReviGatewayCells_TheFixtureNamesTheScriptThatAppliesIt(t *testing.T) {
	const script = "scripts/ops/revi-effort-cells.py"
	if !strings.Contains(strings.Join(readReviGatewayRouting(t).About, " "), script) {
		t.Fatalf("the fixture's about does not name %s", script)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "..", script)); err != nil {
		t.Fatalf("the fixture names %s: %v", script, err)
	}
}
