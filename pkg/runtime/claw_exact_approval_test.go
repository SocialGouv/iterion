package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResumeRefusesUnresolvedApprovalDispatchAfterReopen(t *testing.T) {
	t.Parallel()
	wf := compileGuaranteeBot(t, `workflow w:
  entry: done
  sandbox: none
  worktree: none
`)
	ctx := t.Context()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const id = "interrupted-approval"
	if _, err := s.CreateRun(ctx, id, "w", nil); err != nil {
		t.Fatal(err)
	}
	claim := store.ToolApprovalClaim{InteractionID: "question", Token: "fixture-claim"}
	if err := s.PauseRun(ctx, id, &store.Checkpoint{NodeID: wf.Entry, InteractionID: claim.InteractionID}); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimToolApprovalResume(ctx, id, claim, r.Status, r.CASVersion); err != nil || !ok {
		t.Fatalf("claim: %t, %v", ok, err)
	}
	if ok, err := s.SpendToolApproval(ctx, id, claim); err != nil || !ok {
		t.Fatalf("spend: %t, %v", ok, err)
	}
	// Recover the exact durable boundary a dead process leaves: spend
	// acknowledged, no continuation committed.
	reopened, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.UpdateRunStatus(ctx, id, store.RunStatusFailedResumable, "orphaned"); err != nil {
		t.Fatal(err)
	}
	e := runtime.New(wf, reopened, nil, runtime.WithWorkDir(t.TempDir()), runtime.WithLogger(iterlog.Nop()))
	if err := e.Resume(ctx, id, nil); !runtime.IsAmbiguousEffect(err) {
		t.Fatalf("reopened run must refuse replay as ambiguous, got %v", err)
	}
}

// An operator force-resume is the acknowledged way past an unresolved
// approved dispatch: the node re-runs from its checkpoint, claw asks again
// under a fresh interaction, and the second dispatch is the operator's
// explicit decision — never an automatic replay.
func TestClawAmbiguousApprovalForceResumeReasksUnderAFreshInteraction(t *testing.T) {
	t.Parallel()
	wf := compileGuaranteeBot(t, `prompt p:
  Choose a label.
agent chooser:
  backend: claw
  model: "test/test-model"
  system: p
  user: p
  permission: ask
  tools: [read_file]
workflow w:
  entry: chooser
  worktree: none
  sandbox: none
  chooser -> done
`)
	var count atomic.Int32
	tr := tool.NewRegistry()
	if err := tr.RegisterBuiltin("read_file", "look up a label", json.RawMessage(`{"type":"object"}`), func(_ context.Context, _ json.RawMessage) (string, error) {
		count.Add(1)
		return "cyan", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawTodo(tr); err != nil {
		t.Fatal(err)
	}
	client := &recoveryClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
		switch n {
		case 1, 3:
			return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "label-lookup", Name: "read_file", Input: map[string]any{"path": "labels.json"}}), nil
		case 2:
			return nil, errors.New("provider write failed")
		case 4:
			last := req.Messages[len(req.Messages)-1].Content
			if len(last) != 1 || last[0].ToolUseID != "label-lookup" || len(last[0].Content) != 1 || last[0].Content[0].Text != "cyan" {
				t.Errorf("second dispatch did not feed the approved result: %+v", last)
			}
			return guaranteeEvents(api.ContentBlock{Type: "text", Text: "done"}), nil
		default:
			t.Errorf("unexpected request %d", n)
			return nil, errors.New("unexpected request")
		}
	}}
	reg := model.NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	br := delegate.NewRegistry()
	retry := model.RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond}
	br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, retry))
	wd := t.TempDir()
	ex := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr), model.WithLogger(iterlog.Nop()), model.WithWorkDir(wd), model.WithRetryPolicy(retry))
	t.Cleanup(func() { _ = ex.Close() })
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine := func(force bool) *runtime.Engine {
		opts := []runtime.EngineOption{runtime.WithWorkDir(wd), runtime.WithLogger(iterlog.Nop())}
		if force {
			opts = append(opts, runtime.WithForceResume(true))
		}
		return runtime.New(wf, s, ex, opts...)
	}
	ctx := t.Context()
	const id = "ambiguous-approval"
	if err := engine(false).Run(ctx, id, nil); !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("Run: got %v, want ErrRunPaused", err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	first := r.Checkpoint.InteractionID
	if err := engine(false).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "allow", "_approval_interaction_id": first}); !runtime.IsAmbiguousEffect(err) || count.Load() != 1 {
		t.Fatalf("first dispatch: err=%v count=%d, want ambiguous with one dispatch", err, count.Load())
	}
	r, err = s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.UnresolvedToolApproval != first || r.FailureCode != store.FailureAmbiguousEffect {
		t.Fatalf("unresolved=%q code=%q, want %s/%s", r.UnresolvedToolApproval, r.FailureCode, first, store.FailureAmbiguousEffect)
	}
	if err := engine(false).Resume(ctx, id, nil); err == nil || !strings.Contains(err.Error(), "--force") || count.Load() != 1 {
		t.Fatalf("unacknowledged replay = %v (count %d), want a refusal naming --force", err, count.Load())
	}
	if err := engine(true).Resume(ctx, id, nil); !errors.Is(err, runtime.ErrRunPaused) || count.Load() != 1 {
		t.Fatalf("forced resume: err=%v count=%d, want a fresh pause without a replay", err, count.Load())
	}
	r, err = s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	second := r.Checkpoint.InteractionID
	if second == "" || second == first {
		t.Fatalf("fresh pause interaction = %q, want a new ID distinct from %q", second, first)
	}
	if err := engine(false).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "allow", "_approval_interaction_id": second}); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 || r.UnresolvedToolApproval != "" || r.Status != store.RunStatusFinished {
		t.Fatalf("count=%d unresolved=%q status=%s, want two dispatches, a clean marker and a finished run", count.Load(), r.UnresolvedToolApproval, r.Status)
	}
}

func TestClawExactApprovalRuntime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		afterApprove  func(n int) ([]api.StreamEvent, error)
		wantAmbiguous bool
	}{
		{
			name: "returns actual result",
			afterApprove: func(n int) ([]api.StreamEvent, error) {
				return guaranteeEvents(api.ContentBlock{Type: "text", Text: "done"}), nil
			},
		},
		{
			name: "retries the generation after the approved result",
			afterApprove: func(n int) ([]api.StreamEvent, error) {
				if n == 2 {
					return nil, &api.APIError{Provider: "test", StatusCode: 502, Message: "transient after dispatch", Retryable: true}
				}
				return guaranteeEvents(api.ContentBlock{Type: "text", Text: "done"}), nil
			},
		},
		{
			name: "does not replay an ambiguous dispatch",
			afterApprove: func(n int) ([]api.StreamEvent, error) {
				return nil, errors.New("provider write failed")
			},
			wantAmbiguous: true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wf := compileGuaranteeBot(t, `prompt p:
  Choose a label.
agent chooser:
  backend: claw
  model: "test/test-model"
  system: p
  user: p
  permission: ask
  tools: [read_file]
workflow w:
  entry: chooser
  worktree: none
  sandbox: none
  chooser -> done
`)
			var count atomic.Int32
			tr := tool.NewRegistry()
			if err := tr.RegisterBuiltin("read_file", "look up a label", json.RawMessage(`{"type":"object"}`), func(_ context.Context, raw json.RawMessage) (string, error) {
				count.Add(1)
				if string(raw) != `{"path":"labels.json"}` {
					t.Errorf("tool input = %s", raw)
				}
				return "cyan", nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterClawTodo(tr); err != nil {
				t.Fatal(err)
			}
			client := &recoveryClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				switch n {
				case 1:
					return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "label-lookup", Name: "read_file", Input: map[string]any{"path": "labels.json"}}), nil
				case 2:
					last := req.Messages[len(req.Messages)-1].Content
					if len(last) != 1 || last[0].ToolUseID != "label-lookup" || len(last[0].Content) != 1 || last[0].Content[0].Text != "cyan" {
						t.Errorf("provider did not receive the actual approved result: %+v", last)
					}
					return tc.afterApprove(n)
				default:
					if tc.wantAmbiguous {
						t.Errorf("unexpected repeat request %d", n)
						return nil, errors.New("unexpected repeat request")
					}
					return tc.afterApprove(n)
				}
			}}
			reg := model.NewRegistry()
			reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
			br := delegate.NewRegistry()
			retry := model.RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond}
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, retry))
			wd := t.TempDir()
			ex := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr), model.WithLogger(iterlog.Nop()), model.WithWorkDir(wd), model.WithRetryPolicy(retry))
			t.Cleanup(func() { _ = ex.Close() })
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			engine := func() *runtime.Engine {
				return runtime.New(wf, s, ex, runtime.WithWorkDir(wd), runtime.WithLogger(iterlog.Nop()))
			}
			ctx := t.Context()
			const id = "label-approval"
			if err := engine().Run(ctx, id, nil); !errors.Is(err, runtime.ErrRunPaused) || count.Load() != 0 {
				t.Fatalf("expected pause before tool dispatch: count=%d err=%v", count.Load(), err)
			}
			r, err := s.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if r.Checkpoint == nil || r.Checkpoint.InteractionID == "" {
				t.Fatal("missing pending approval")
			}
			interaction := r.Checkpoint.InteractionID
			for _, staleID := range []string{"", "previous-question"} {
				if err := engine().Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "allow", "_approval_interaction_id": staleID}); err == nil || !strings.Contains(err.Error(), "approval does not match the pending interaction") || count.Load() != 0 {
					t.Fatalf("unbound decision was accepted: count=%d err=%v", count.Load(), err)
				}
			}
			err = engine().Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "allow", "_approval_interaction_id": interaction})
			if tc.wantAmbiguous {
				if !runtime.IsAmbiguousEffect(err) {
					t.Fatalf("expected ambiguous outcome after dispatch, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			r, err = s.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			wantUnresolved := ""
			if tc.wantAmbiguous {
				wantUnresolved = interaction
			}
			if r.UnresolvedToolApproval != wantUnresolved {
				t.Fatalf("unresolved approval = %q, want %q", r.UnresolvedToolApproval, wantUnresolved)
			}
			if count.Load() != 1 || len(r.SpentToolApprovals) != 1 || r.SpentToolApprovals[0] != interaction {
				t.Fatalf("tool ran %d times; spent=%v, want %s", count.Load(), r.SpentToolApprovals, interaction)
			}
		})
	}
}
