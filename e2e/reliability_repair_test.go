package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Only the provider's answer is scripted. The executor runs each returned
// command, checks the real postcondition and applies max_repair_attempts.
type reliabilityRepairProvider struct {
	mu      sync.Mutex
	calls   int
	command string
}

func (p *reliabilityRepairProvider) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	body, err := json.Marshal(map[string]any{"corrected_command": p.command})
	if err != nil {
		return nil, err
	}
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 1},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "repair", Name: "structured_output"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: string(body)}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 1}},
		{Type: api.EventMessageStop},
	}
	out := make(chan api.StreamEvent, len(events))
	for _, event := range events {
		out <- event
	}
	close(out)
	return out, nil
}

func (p *reliabilityRepairProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func reliabilityBoundedRepair(t *testing.T) {
	for _, tc := range []struct {
		policy string
		limit  int
		want   int
	}{{"required", 2, 0}, {"recover", 1, 1}, {"recover", 2, 2}} {
		t.Run(fmt.Sprintf("%s_limit_%d", tc.policy, tc.limit), func(t *testing.T) {
			ws := t.TempDir()
			source := fmt.Sprintf(`tool repair:
  command: "printf x >> recipes"
  postcondition: "false"
  goal: "fixture with an impossible postcondition"
  policy: %s
  recovery:
    model: "reliability_probe/fake"
    max_repair_attempts: %d
workflow reliability_repair:
  worktree: none
  sandbox: none
  entry: repair
  repair -> done
`, tc.policy, tc.limit)
			wf := compileSource(t, "repair.bot", source)
			provider := &reliabilityRepairProvider{command: "printf x >> repairs"}
			reg := model.NewRegistry()
			reg.Register("reliability_probe", func(string) (api.APIClient, error) { return provider, nil })
			exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithLogger(iterlog.Nop()), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			s := tmpStore(t)
			err := newEngine(t, wf, s, exec, runtime.WithWorkDir(ws), runtime.WithLogger(iterlog.Nop())).Run(t.Context(), "repair", nil)
			row, loadErr := s.LoadRun(t.Context(), "repair")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			recipes, recipeErr := os.ReadFile(filepath.Join(ws, "recipes"))
			repairs, repairErr := os.ReadFile(filepath.Join(ws, "repairs"))
			if recipeErr != nil || (tc.want > 0 && repairErr != nil) {
				t.Fatalf("read real shell effects: recipe=%v repair=%v", recipeErr, repairErr)
			}
			if provider.count() != tc.want || len(repairs) != tc.want || string(recipes) != "x" {
				t.Fatalf("repair exceeded or skipped its shipped bound: provider=%d effects=%d recipe=%q policy=%s limit=%d", provider.count(), len(repairs), recipes, tc.policy, tc.limit)
			}
			if err == nil || row.Status != store.RunStatusFailedResumable {
				t.Fatalf("unmet postcondition was accepted: status=%s err=%v", row.Status, err)
			}
		})
	}
}

type reliabilitySchemaBackend struct {
	mu      sync.Mutex
	calls   int
	succeed bool
}

func (b *reliabilitySchemaBackend) Execute(context.Context, delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	output := map[string]any{}
	if b.succeed && b.calls == 2 {
		output["value"] = "valid"
	}
	return delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "fixture-session", Output: output}, nil
}

func (b *reliabilitySchemaBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func reliabilitySchemaReask(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		t.Run(fmt.Sprintf("valid_on_reask_%v", succeed), func(t *testing.T) {
			wf := compileSource(t, "reask.bot", `schema answer:
  value: string
agent answer:
  backend: claude_code
  model: "fixture"
  output: answer
  publish: checked
workflow reliability_reask:
  worktree: none
  sandbox: none
  entry: answer
  answer -> done
`)
			backend := &reliabilitySchemaBackend{succeed: succeed}
			backends := delegate.NewRegistry()
			backends.Register(delegate.BackendClaudeCode, backend)
			ws := t.TempDir()
			exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(backends), model.WithWorkDir(ws), model.WithLogger(iterlog.Nop()), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			s := tmpStore(t)
			err := newEngine(t, wf, s, exec, runtime.WithWorkDir(ws), runtime.WithLogger(iterlog.Nop()), runtime.WithOutputValidation(true)).Run(t.Context(), "reask", nil)
			if backend.count() != 2 {
				t.Fatalf("semantic schema reask is not exactly one additional call: %d", backend.count())
			}
			row, loadErr := s.LoadRun(t.Context(), "reask")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			art, artErr := s.LoadArtifact(t.Context(), "reask", "answer", 0)
			if succeed {
				if err != nil || row.Status != store.RunStatusFinished || artErr != nil || art.Data["value"] != "valid" {
					t.Fatalf("valid reask did not finish and publish: row=%+v art=%+v err=%v artifactErr=%v", row, art, err, artErr)
				}
			} else if err == nil || row.Status != store.RunStatusFailedResumable || artErr == nil {
				t.Fatalf("identical invalid reask did not stop before publication: status=%s err=%v artifact=%+v", row.Status, err, art)
			}
		})
	}
}
