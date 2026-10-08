package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const contentPH = "__ITERION_SECRET_hook_key__"

// The scrubbed events keep what they carry, in placeholder form — a scrub
// that emptied them, or a refactor that dropped the field, passes every
// round-13 witness (they assert the value's absence only).
func TestReviewAndAnswerEventsKeepTheirContent(t *testing.T) {
	t.Run("review_verdict", func(t *testing.T) {
		e, s := newScrubbingEngine(t, "verdict-run")
		e.emitReviewTurn(context.Background(), "verdict-run", "gate", "companion", 2, map[string]any{
			"decision": "changes_requested",
			"blockers": []any{"config.yml:3 commits the hook key " + answersKey},
		})
		for _, ev := range eventsOfType(t, s, "verdict-run", store.EventReviewVerdict) {
			if !strings.Contains(ev, "commits the hook key "+contentPH) || !strings.Contains(ev, "changes_requested") {
				t.Errorf("review_verdict lost its content: %s", ev)
			}
		}
	})
	t.Run("gate_answers", func(t *testing.T) {
		ctx := context.Background()
		e, s := newScrubbingEngine(t, "gate-run")
		if err := s.WriteInteraction(ctx, &store.Interaction{ID: "gate-ix", RunID: "gate-run", NodeID: "gate"}); err != nil {
			t.Fatal(err)
		}
		rs := e.newRunState("gate-run", nil)
		rs.ctx = ctx
		hn, _ := e.workflow.Nodes["gate"].(*ir.HumanNode)
		verdict := map[string]any{"decision": "changes_requested", "blockers": []any{"rotate " + answersKey}}
		if next, err := e.gateSelectEdge(ctx, rs, hn, "gate", "gate-ix", verdict); err != nil || next != "done" {
			t.Fatalf("scenario broken: next=%q err=%v", next, err)
		}
		for _, ev := range eventsOfType(t, s, "gate-run", store.EventHumanAnswersRecorded) {
			if !strings.Contains(ev, "rotate "+contentPH) || !strings.Contains(ev, `"interaction_id":"gate-ix"`) {
				t.Errorf("human_answers_recorded (gate) lost its content: %s", ev)
			}
		}
	})
	t.Run("resume_answers", func(t *testing.T) {
		ctx := context.Background()
		e, s := newScrubbingEngine(t, "answers-run")
		r, err := s.LoadRun(ctx, "answers-run")
		if err != nil {
			t.Fatal(err)
		}
		cp := &store.Checkpoint{NodeID: "gate", InteractionID: "answers-ix", InteractionQuestions: map[string]any{"note": "What changed?"}}
		if _, err := e.recordHumanAnswers(ctx, r, cp, map[string]any{"note": "rotated " + answersKey}); err != nil {
			t.Fatal(err)
		}
		for _, ev := range eventsOfType(t, s, "answers-run", store.EventHumanAnswersRecorded) {
			if !strings.Contains(ev, "rotated "+contentPH) || !strings.Contains(ev, `"interaction_id":"answers-ix"`) {
				t.Errorf("human_answers_recorded (resume) lost its content: %s", ev)
			}
		}
	})
}

func eventLinesOfType(t *testing.T, storeDir, typ string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() || filepath.Base(p) != "events.jsonl" {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, line := range strings.Split(string(b), "\n") {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) == nil && ev["type"] == typ {
				out = append(out, line)
			}
		}
		return nil
	})
	return out
}

// The pause events keep their interaction id (the studio keys an agent
// pause, and the async question, on it) and a branch pause its extras.
func TestPauseEventsKeepTheirIDAndExtras(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	for _, c := range []struct {
		name, bot, runID, want string
	}{
		{"trunk", humanGateInstructionsBot, "instr-run", "Check what the build read before approving"},
		{"branch", humanGateBranchBot, "branch-run", "Check what the build read before approving."},
	} {
		t.Run(c.name, func(t *testing.T) {
			wf := compileBotText(t, c.bot)
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("db_password: "+secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
			exec := model.NewClawExecutor(nil, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard))
			t.Cleanup(func() { _ = exec.Close() })
			storeDir := t.TempDir()
			s, serr := store.New(storeDir)
			if serr != nil {
				t.Fatal(serr)
			}
			if err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), c.runID, nil); err == nil {
				t.Fatal("scenario broken: no pause")
			}
			lines := eventLinesOfType(t, storeDir, "human_input_requested")
			if len(lines) == 0 {
				t.Fatal("scenario broken: no human_input_requested")
			}
			for _, line := range lines {
				var ev struct {
					Data map[string]any `json:"data"`
				}
				_ = json.Unmarshal([]byte(line), &ev)
				if id, _ := ev.Data["interaction_id"].(string); id == "" {
					t.Errorf("the pause event has no interaction_id: %s", line)
				}
				if instr, _ := ev.Data["instructions"].(string); !strings.Contains(instr, c.want) {
					t.Errorf("the pause event lost its instructions: %s", line)
				}
			}
		})
	}
}

// The router's reasoning reaches the event log in placeholder form, one
// route or several — not dropped.
func TestARoutersReasoningKeepsItsText(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	t.Setenv("ROUTER_HOOK_KEY", key)
	reasoning := "The reviewer found the live key " + key + " hard-coded in config.yml, so rotate."
	for _, c := range []struct {
		name, join, multi, edges string
		route                    map[string]any
		lanes                    int
	}{
		{"single", "", "", "  lane_a -> done\n  lane_b -> done\n", map[string]any{"selected_route": "lane_a", "reasoning": reasoning}, 1},
		{"multi", "agent join:\n  backend: claw\n  model: \"anthropic/claude-sonnet-4-6\"\n  user: work\n  await: wait_all\n\n", "  multi: true\n", "  lane_a -> join\n  lane_b -> join\n  join -> done\n", map[string]any{"selected_routes": []any{"lane_a", "lane_b"}, "reasoning": reasoning}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			wf := compileBotText(t, fmt.Sprintf(routerSecretBot, c.join, c.multi, c.edges))
			ws := t.TempDir()
			guard := secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: key}}, secretguard.DefaultConfig())
			route, _ := json.Marshal(c.route)
			scripts := [][]api.StreamEvent{
				routerTextEvents("config.yml line 3 holds the live hook key " + key + " in clear."),
				askToolUse("r1", "structured_output", string(route)),
			}
			for range c.lanes {
				scripts = append(scripts, routerTextEvents("rotated"))
			}
			mock := &askMock{scripts: scripts}
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			tr := tool.NewRegistry()
			_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
			storeDir := t.TempDir()
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			hooks := model.NewStoreEventHooks(context.Background(), s, "router-run", nil, guard, nil)
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{MaxAttempts: 1}))
			exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard), model.WithToolRegistry(tr),
				model.WithBackendRegistry(br), model.WithEventHooks(hooks), model.WithDefaultBackend(delegate.BackendClaw))
			t.Cleanup(func() { _ = exec.Close() })
			if err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "router-run", nil); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			found := false
			for _, line := range eventLinesOfType(t, storeDir, "node_finished") {
				if strings.Contains(line, `"reasoning"`) && strings.Contains(line, "The reviewer found the live key "+contentPH+" hard-coded") {
					found = true
				}
			}
			if !found {
				t.Errorf("no router node_finished carries its reasoning in placeholder form")
			}
		})
	}
}

// The async question's event carries the question — in placeholder form
// with a guard, as asked without one.
func TestAnAsyncQuestionsEventKeepsTheQuestion(t *testing.T) {
	const key = "hk-live-9f8e7d6c5b4a3210-FAKE"
	for _, withGuard := range []bool{true, false} {
		t.Run(fmt.Sprint("guard=", withGuard), func(t *testing.T) {
			wf := compileBotText(t, strings.Replace(asyncAskBot, " {{secrets.hook_key}}", "", 1))
			ws := t.TempDir()
			in, _ := json.Marshal(map[string]any{"question": "I read " + key + " in config.yml: rotate it?"})
			mock := &askMock{scripts: [][]api.StreamEvent{askToolUse("t1", delegate.AskUserAsyncToolName, string(in)), asyncTextEvents("posted; carrying on")}}
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			tr := tool.NewRegistry()
			if err := tool.RegisterAskUser(tr, nil); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterAsyncAsk(tr); err != nil {
				t.Fatal(err)
			}
			_ = tr.RegisterBuiltin("todo_write", "todo_write", json.RawMessage(`{"type":"object"}`), func(context.Context, json.RawMessage) (string, error) { return "ok", nil })
			storeDir := t.TempDir()
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			var guard *secretguard.Guard
			opts := []model.ClawExecutorOption{}
			if withGuard {
				guard = secretguard.New([]secretguard.Secret{{Name: "hook_key", Value: key}}, secretguard.DefaultConfig())
				opts = append(opts, model.WithSecretGuard(guard))
			}
			hooks := model.NewStoreEventHooks(context.Background(), s, "async-run", nil, guard, nil)
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{MaxAttempts: 1}))
			opts = append(opts, model.WithWorkDir(ws), model.WithToolRegistry(tr),
				model.WithBackendRegistry(br), model.WithEventHooks(hooks), model.WithDefaultBackend(delegate.BackendClaw),
				model.WithExecutorAsyncAsk(&model.StoreAsyncAskBinder{Store: s}))
			exec := model.NewClawExecutor(reg, wf, opts...)
			t.Cleanup(func() { _ = exec.Close() })
			if err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "async-run", nil); err != nil {
				t.Fatalf("scenario broken: %v", err)
			}
			want := "I read " + key + " in config.yml"
			if withGuard {
				want = "I read " + contentPH + " in config.yml"
			}
			lines := eventLinesOfType(t, storeDir, "human_input_requested")
			if len(lines) == 0 {
				t.Fatal("scenario broken: no async human_input_requested")
			}
			for _, line := range lines {
				if !strings.Contains(line, want) || !strings.Contains(line, `"interaction_id":"`) {
					t.Errorf("the async question's event lost the question (want %q): %s", want, line)
				}
			}
		})
	}
}
