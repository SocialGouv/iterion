package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestClawRefusesAnInertCapabilityGrant pins the typed refusal behind #3283
// S9 (backend parity: wired or typed-refused). On claw the appends that
// serve a capabilities grant (board, runs read) ride an already-restricted
// NON-EMPTY `tools:` union, so a node whose tool surface resolves EMPTY —
// no `tools:` line, or a declared-but-empty `tools: []` — dispatches with
// the grant inert: it answers as if the grant were absent, which is exactly
// the silent shape review-pr's merge shipped as on its gateway route (F4:
// tool-less `converge` on claw, no report, no board issue). The refusal
// reads the surface claw's wire serves — ToolDefs, through
// clawServesToolDefs — never AllowedTools (the CLI backends' allowlist) nor
// the nilness of the declaration (toolcatalog.ToolsDeclared), which
// disagrees with the surface exactly on `tools: []`. Refused by type at the
// claw backend, one route's failure, so the node's fallbacks can serve it
// like any other ErrCapabilityUnsupported here. The served arms assert the
// first model request carries the task's ToolDefs; an `interaction:` node
// is served because its ask_user rides those ToolDefs.
//
// Mutations that redden it: drop the inert-grant refusal from
// ClawBackend.Execute (refused arms run the loop instead); gate it back on
// the declaration's nilness instead of the empty surface (the
// declared-but-empty arm reddens); add `len(task.AllowedTools) == 0` back
// to the gate (the list-the-wire-never-serves arm reddens); make
// clawServesToolDefs answer false (the served arms redden); add
// `!task.InteractionEnabled` back to the gate (the
// interaction-without-ToolDefs arm reddens).
func TestClawRefusesAnInertCapabilityGrant(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) {
		return &execMockClient{streams: []<-chan api.StreamEvent{streamOf(textEvents("done", 100, 20))}}, nil
	})
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})

	refused := func(t *testing.T, task delegate.Task) *delegate.ErrCapabilityUnsupported {
		t.Helper()
		_, err := backend.Execute(context.Background(), task)
		var typed *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &typed) {
			t.Fatalf("err = %v, want an ErrCapabilityUnsupported for a grant the node cannot receive on claw", err)
		}
		return typed
	}

	t.Run("board grant on a listless node", func(t *testing.T) {
		t.Parallel()
		typed := refused(t, delegate.Task{
			NodeID: "merge", Model: "test/m", UserPrompt: "x",
			Capabilities: []string{"board.read", "board.create", "board.label"},
		})
		if !strings.Contains(typed.Error(), "board") {
			t.Errorf("the refusal names no capability class: %v", typed)
		}
		if !strings.Contains(typed.Remedy, "a non-empty tools: list") {
			t.Errorf("the remedy does not name the non-empty surface: %v", typed.Remedy)
		}
	})

	t.Run("runs-read grant on a listless node", func(t *testing.T) {
		t.Parallel()
		typed := refused(t, delegate.Task{
			NodeID: "watch", Model: "test/m", UserPrompt: "x",
			Capabilities: []string{"runs.read"},
		})
		if !strings.Contains(typed.Error(), "runs read") {
			t.Errorf("the refusal does not name the runs-read class: %v", typed)
		}
	})

	t.Run("both grants name both classes", func(t *testing.T) {
		t.Parallel()
		typed := refused(t, delegate.Task{
			NodeID: "both", Model: "test/m", UserPrompt: "x",
			Capabilities: []string{"board.read", "runs.read"},
		})
		if !strings.Contains(typed.Error(), "board and runs read") {
			t.Errorf("the refusal under-names the grants it refuses: %v", typed)
		}
	})

	t.Run("a declared-but-empty list is refused too", func(t *testing.T) {
		t.Parallel()
		// `tools: []` IS a declaration (toolcatalog.ToolsDeclared is
		// nilness), and the capability appends gate on the NON-EMPTY union —
		// so a declared-empty surface holds the grant exactly as inert as an
		// undeclared one. The refusal reads the surface the dispatch would
		// serve, never the declaration: a regressed gate back to nilness
		// reddens here.
		typed := refused(t, delegate.Task{
			NodeID: "merge", Model: "test/m", UserPrompt: "x",
			Capabilities:  []string{"board.read", "board.create", "board.label"},
			ToolsDeclared: true,
		})
		if !strings.Contains(typed.Error(), "board") {
			t.Errorf("the refusal names no capability class: %v", typed)
		}
	})

	t.Run("the executor's schema re-ask is served", func(t *testing.T) {
		t.Parallel()
		// planSchemaReask's claw continuation nils ToolDefs and AllowedTools
		// while KEEPING the node's capabilities (executor_reask.go: "tools
		// off: the model is asked for the answer it already has") — the
		// standard in-place recovery of every granted node with an output
		// schema. A tool-less continuation needs no grant served by design,
		// so the gate's premise (the appends did not fire) does not apply to
		// a surface the executor deliberately removed; without the
		// exemption the refusal — non-retryable, unmatched by fallback
		// classification — converts that recovery into a node failure.
		served := NewClawBackend(servedRegistry(t, textEvents("because", 100, 20)), EventHooks{}, RetryPolicy{})
		_, err := served.Execute(context.Background(), delegate.Task{
			NodeID: "merge", Model: "test/m", UserPrompt: "say why",
			Capabilities:  []string{"board.read", "board.create", "board.label"},
			ToolsDeclared: true,
			ContinueConversation: mustMarshalMessages(t, []api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: "the completed conversation"}},
			}}),
		})
		var typed *delegate.ErrCapabilityUnsupported
		if errors.As(err, &typed) {
			t.Fatalf("the schema re-ask of a granted node was refused: %v — the executor removed that surface deliberately", typed)
		}
		if err != nil {
			t.Fatalf("the re-ask arm errored otherwise: %v", err)
		}
	})

	t.Run("a list the wire never serves is refused", func(t *testing.T) {
		t.Parallel()
		// AllowedTools is the CLI backends' allowlist; claw's wire builds
		// the request's tools from ToolDefs alone, so a grant riding a list
		// without ToolDefs reaches the model tool-less. Refused before any
		// model request is spent.
		mock := newMockClient(textEvents("merged", 100, 20))
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
		_, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{}).Execute(context.Background(), delegate.Task{
			NodeID: "merge", Model: "test/m", UserPrompt: "x",
			Capabilities:  []string{"board.read", "board.create", "board.label"},
			AllowedTools:  []string{"read_file", "write_file", "glob", "todo_write", "mcp__iterion_board__list_issues"},
			ToolsDeclared: true, HasTools: true, ToolMaxSteps: 3,
		})
		var typed *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &typed) {
			t.Fatalf("err = %v, want an ErrCapabilityUnsupported: the grant rides a list claw's wire never serves", err)
		}
		if calls := mock.getCalls(); len(calls) != 0 {
			t.Fatalf("the refused task spent %d model request(s), want none", len(calls))
		}
	})

	t.Run("a declared surface is served", func(t *testing.T) {
		t.Parallel()
		defs := []delegate.ToolDef{servedToolDef("read_file"), servedToolDef("mcp_iterion_board_list_issues")}
		offered := servedSurface(t, delegate.Task{
			NodeID: "merge", Model: "test/m", UserPrompt: "x",
			Capabilities:  []string{"board.read", "board.create", "board.label"},
			ToolDefs:      defs,
			ToolsDeclared: true, HasTools: true, ToolMaxSteps: 3,
		})
		if !slices.Equal(offered, []string{"read_file", "mcp_iterion_board_list_issues"}) {
			t.Fatalf("the first model request offered %v, want the task's ToolDefs [read_file mcp_iterion_board_list_issues]", offered)
		}
	})

	t.Run("an interaction node with no ToolDefs is refused", func(t *testing.T) {
		t.Parallel()
		// An interaction node passes the gate through the ask_user tool its
		// ToolDefs carry, never through the interaction flag: with no
		// ToolDefs the grant is as inert as on any listless node.
		mock := newMockClient(textEvents("asked", 100, 20))
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
		_, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{}).Execute(context.Background(), delegate.Task{
			NodeID: "asker", Model: "test/m", UserPrompt: "x",
			Capabilities:       []string{"board.read"},
			InteractionEnabled: true, HasTools: true, ToolMaxSteps: 3,
		})
		var typed *delegate.ErrCapabilityUnsupported
		if !errors.As(err, &typed) {
			t.Fatalf("err = %v, want an ErrCapabilityUnsupported: the interaction flag serves no grant without ToolDefs", err)
		}
		if calls := mock.getCalls(); len(calls) != 0 {
			t.Fatalf("the refused task spent %d model request(s), want none", len(calls))
		}
	})

	t.Run("interaction grants are served too", func(t *testing.T) {
		t.Parallel()
		offered := servedSurface(t, delegate.Task{
			NodeID: "asker", Model: "test/m", UserPrompt: "x",
			Capabilities:       []string{"board.read"},
			ToolDefs:           []delegate.ToolDef{servedToolDef(askUserToolName)},
			InteractionEnabled: true, HasTools: true, ToolMaxSteps: 3,
		})
		if !slices.Equal(offered, []string{askUserToolName}) {
			t.Fatalf("the first model request offered %v, want the interaction node's ToolDefs [%s]", offered, askUserToolName)
		}
	})
}

// servedToolDef is a no-op tool the served arms hand claw by name.
func servedToolDef(name string) delegate.ToolDef {
	return delegate.ToolDef{
		Name:        name,
		Description: name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}
}

// servedSurface runs task on claw against a scripted text answer and
// returns the tool names its first model request offered. A refusal or any
// other error fails the test: the arms calling it expect the grant served.
func servedSurface(t *testing.T, task delegate.Task) []string {
	t.Helper()
	mock := newMockClient(textEvents("done", 100, 20), textEvents("done", 100, 20), textEvents("done", 100, 20))
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	_, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{}).Execute(context.Background(), task)
	var typed *delegate.ErrCapabilityUnsupported
	if errors.As(err, &typed) {
		t.Fatalf("a served surface was refused: %v", typed)
	}
	if err != nil {
		t.Fatalf("the served arm errored otherwise: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) == 0 {
		t.Fatal("the served arm sent no model request")
	}
	names := make([]string, 0, len(calls[0].Tools))
	for _, tl := range calls[0].Tools {
		names = append(names, tl.Name)
	}
	return names
}

// servedRegistry is a Registry serving one text answer on "test/m": enough
// for the served arms to run the loop to completion.
func servedRegistry(t *testing.T, evs []api.StreamEvent) *Registry {
	t.Helper()
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) {
		return &execMockClient{streams: []<-chan api.StreamEvent{streamOf(evs)}}, nil
	})
	return reg
}

// convergeMergeModel is the scripted merge step: one write_file call (the
// report), then the structured emit_output. It records what claw offered and
// sent, so the test can read the surface and the prompts.
type convergeMergeModel struct {
	ws     string
	mu     sync.Mutex
	next   int
	seen   converseSeen
	report string
}

func (m *convergeMergeModel) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	reportPath := filepath.Join(m.ws, ".review-pr", "findings.md")
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	if m.seen.offered == nil {
		m.seen.offered = names
	}
	if m.seen.system == "" {
		var sys strings.Builder
		for _, b := range req.SystemBlocks {
			sys.WriteString(b.Text)
		}
		if sys.Len() == 0 {
			sys.WriteString(req.System)
		}
		m.seen.system = sys.String()
	}
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		for _, block := range msg.Content {
			if block.Type == "text" && m.seen.user == "" {
				m.seen.user = block.Text
			}
		}
	}
	if m.seen.schema == nil {
		m.seen.schema = outputSchemaFields(m.seen.user)
	}
	out := map[string]any{
		"report_path":        reportPath,
		"total_findings":     1,
		"issues_created":     []string{},
		"failed_issues":      []string{},
		"findings":           []any{map[string]any{"file": "app.go", "line": 3, "line_end": 3, "severity": "high", "category": "correctness", "title": "A returns 2", "detail": "the change rewrites A", "suggestion": "keep 1", "replacement": "", "confidence": "high", "reviewers": "both"}},
		"questions":          "",
		"ticket_conformance": "",
	}
	b, _ := json.Marshal(out)
	var evs []api.StreamEvent
	switch {
	case slices.Contains(names, "write_file") && m.next == 0:
		in, _ := json.Marshal(map[string]any{"path": reportPath, "content": m.report})
		evs = toolUseEvents("converge_report", "write_file", string(in), 100, 20)
		m.next++
	case slices.Contains(names, "structured_output"):
		evs = toolUseEvents("converge_output", "structured_output", string(b), 100, 20)
	default:
		evs = textEvents(string(b), 100, 20)
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (m *convergeMergeModel) observed() converseSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.seen
	s.offered = slices.Clone(m.seen.offered)
	return s
}

// TestReviewPrConvergeMergesOnClawThroughItsDeclaredSurface is the S9 answer
// witness (D7, #3283): the merge step, whose gateway cells route it to claw,
// DECLARES the surface it merges through — read_file loads the board skill,
// write_file writes the report, glob lists a directory — and on claw that
// list is dispatched whole. The node as main.bot compiles it runs through the
// ClawExecutor with a routing override onto claw against a scripted model:
// the model calls write_file, the report lands under `report_path`, and the
// node's output carries the findings array and the path — the shape the
// pre-S9 dispatch lost (F4: tool_count 0, no report, the capability grant
// inert).
//
// Mutation that reddens it: drop converge's `tools:` list from
// bots/review-pr/lib/nodes.bot — the inert-grant refusal fires and the merge
// never writes its report.
func TestReviewPrConvergeMergesOnClawThroughItsDeclaredSurface(t *testing.T) {
	t.Parallel()
	u := unittest.Whole(unit.LoadDir(filepath.Join(reviewPrBundle, "main.bot")))
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile review-pr: %v", compiled.Diagnostics)
	}
	wf := compiled.Workflow
	node, ok := wf.Nodes["converge"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("converge is %T, want an agent node", wf.Nodes["converge"])
	}
	ws := t.TempDir()
	report := "# Code review — Revi\n\n1 finding.\n"
	mdl := &convergeMergeModel{ws: ws, report: report}
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mdl, nil })
	tools := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithEnv(tools, ws, []string{"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawTodo(tools); err != nil {
		t.Fatal(err)
	}
	// The board capability's append names the host board tools; the run's
	// registry serves them from the native store, as a run's does.
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = board.Close() })
	if err := tool.RegisterClawBoardTools(tools, &tool.BoardConfig{Store: board, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatal(err)
	}
	var onClaw ModelOverrides
	onClaw.SetBackend(node.ID, delegate.BackendClaw)
	onClaw.SetModel(node.ID, "anthropic/claude-opus-5-5")
	onClaw.SetProvider(node.ID, "auto")
	e := newTestClawExecutor(reg, wf, WithWorkDir(ws), WithToolRegistry(tools),
		WithModelOverrides(onClaw), WithLogger(iterlog.Nop()))
	e.SetVars(map[string]any{
		"workspace_dir": ws, "report_path": ".review-pr/findings.md", "review_tier": "guard",
		"scope_notes": "", "prior_pushback": "", "ticket_context": "auto",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := e.Execute(ctx, node, map[string]any{})
	if err != nil {
		t.Fatalf("converge on claw: %v", err)
	}

	seen := mdl.observed()
	// The surface claw offered: the declared three plus todo_write — the
	// shell never rides a declared list, and no MCP tool rides a node that
	// inherits none.
	for _, want := range []string{"read_file", "write_file", "glob", "todo_write"} {
		if !slices.Contains(seen.offered, want) {
			t.Errorf("the loop's model was offered %v, which lacks the declared %q", seen.offered, want)
		}
	}
	for _, name := range seen.offered {
		// The board tools ride the capability append on top of the declared
		// list (the merge files issues); everything else outside the three
		// declared file tools — the shell first — does not.
		if name == "bash" || (strings.HasPrefix(name, "mcp_") && !strings.HasPrefix(name, "mcp_iterion_board_")) || (strings.HasPrefix(name, "mcp.") && !strings.HasPrefix(name, "mcp.iterion_board.")) {
			t.Errorf("the merge was offered %q — outside its declared surface", name)
		}
	}

	// The report EXISTS — the F4 absence — and the output carries it.
	reportPath := filepath.Join(ws, ".review-pr", "findings.md")
	if out["report_path"] != reportPath {
		t.Errorf("output report_path = %v, want %q — the merge published nothing on the pre-S9 route", out["report_path"], reportPath)
	}
	written, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read the report the merge wrote: %v", err)
	}
	if string(written) != report {
		t.Errorf("the report on disk reads %q, want what the merge wrote (%q)", written, report)
	}
	// The schema's `json` field decodes to the value itself, not a string.
	findings, ok := out["findings"].([]any)
	if !ok {
		t.Errorf("output findings is %T, want a decoded array (%v)", out["findings"], out["findings"])
	} else if len(findings) != 1 {
		t.Errorf("output findings carries %d entries, want the merged one", len(findings))
	}
}
