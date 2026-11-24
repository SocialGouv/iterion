package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/task"
	"github.com/SocialGouv/iterion/pkg/claw/internal/tools"
)

// stubStreamClient answers every request with a scripted text-only turn.
type stubStreamClient struct {
	text string
	seen chan []api.Tool
}

func (s *stubStreamClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	if s.seen != nil {
		s.seen <- append([]api.Tool(nil), req.Tools...)
	}
	ch := make(chan api.StreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- api.StreamEvent{Type: api.EventContentBlockStart, Index: 0,
			ContentBlock: api.ContentBlockInfo{Type: "text"}}
		ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0,
			Delta: api.Delta{Type: "text_delta", Text: s.text}}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn"}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()
	return ch, nil
}

type imageScopeClient struct{ *stubStreamClient }

func (c *imageScopeClient) GenerateImage(_ context.Context, _ string) (api.GeneratedImage, error) {
	return api.GeneratedImage{}, nil
}

func TestSubagentImageGenAllowLists(t *testing.T) {
	seen := make(chan []api.Tool, 4)
	client := &imageScopeClient{&stubStreamClient{text: "done", seen: seen}}
	parent := NewConversationLoop(&Config{ProviderName: "openai", Model: "gpt-5.5"}, client)
	parent.TaskRegistry = task.NewRegistry()
	for _, def := range []SubagentDef{
		{Name: "reader", SystemPrompt: "Read only.", AllowedTools: []string{"read_file"}},
		{Name: "imager", SystemPrompt: "Create images.", AllowedTools: []string{"image_gen"}},
	} {
		if err := parent.DefineSubagent(def); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{"explore", false},
		{"reader", false},
		{"general-purpose", true},
		{"imager", true},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			resolved := false
			for _, tool := range parent.resolveSubagentTools(tc.kind) {
				resolved = resolved || tool.Name == "image_gen"
			}
			if resolved != tc.want {
				t.Fatalf("image_gen in resolved tools = %v, want %v", resolved, tc.want)
			}
			created, err := parent.spawnSubagent(&tools.AgentSpec{Prompt: "describe the task", Description: "test image scope", Name: tc.kind, SubagentType: tc.kind}, false)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case offered := <-seen:
				found := false
				for _, tool := range offered {
					found = found || tool.Name == "image_gen"
				}
				if found != tc.want {
					t.Fatalf("image_gen offered = %v, want %v", found, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("subagent never requested tools")
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				current, ok := parent.TaskRegistry.Get(created.TaskID)
				if ok && current.Status.IsTerminal() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("subagent did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestDefineSubagentValidation(t *testing.T) {
	loop := &ConversationLoop{Config: &Config{}}

	if err := loop.DefineSubagent(SubagentDef{Name: "", SystemPrompt: "x"}); err == nil {
		t.Error("empty name must error")
	}
	if err := loop.DefineSubagent(SubagentDef{Name: "reviewer", SystemPrompt: " "}); err == nil {
		t.Error("empty system prompt must error")
	}
	if err := loop.DefineSubagent(SubagentDef{Name: "explore", SystemPrompt: "x"}); err == nil {
		t.Error("shadowing a built-in type must error")
	}
	if err := loop.DefineSubagent(SubagentDef{Name: "reviewer", SystemPrompt: "You review."}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range loop.SubagentTypes() {
		if name == "reviewer" {
			found = true
		}
	}
	if !found {
		t.Error("defined type must be listed")
	}
}

func TestResolveSubagentTools(t *testing.T) {
	loop := &ConversationLoop{
		Config: &Config{},
		Tools: []api.Tool{
			{Name: "bash"}, {Name: "read_file"}, {Name: "grep"},
			{Name: "agent"}, {Name: "define_subagent"}, {Name: "task_stop"},
		},
	}

	names := func(ts []api.Tool) map[string]bool {
		m := map[string]bool{}
		for _, x := range ts {
			m[x.Name] = true
		}
		return m
	}

	// general-purpose: everything minus orchestration tools.
	got := names(loop.resolveSubagentTools("general-purpose"))
	if !got["bash"] || !got["read_file"] || got["agent"] || got["define_subagent"] || got["task_stop"] {
		t.Errorf("general-purpose resolution wrong: %v", got)
	}

	// explore: built-in read-only set.
	got = names(loop.resolveSubagentTools("explore"))
	if got["bash"] || !got["read_file"] || !got["grep"] {
		t.Errorf("explore resolution wrong: %v", got)
	}

	// custom def with an allow-list.
	if err := loop.DefineSubagent(SubagentDef{Name: "reader", SystemPrompt: "read", AllowedTools: []string{"read_file", "agent"}}); err != nil {
		t.Fatal(err)
	}
	got = names(loop.resolveSubagentTools("reader"))
	if !got["read_file"] || len(got) != 1 {
		t.Errorf("custom allow-list must filter (and never include orchestration tools): %v", got)
	}
}

func TestSpawnSubagentEndToEnd(t *testing.T) {
	loop := &ConversationLoop{
		Client:       &stubStreamClient{text: "subagent report: all good"},
		Session:      NewSession(),
		Config:       &Config{Model: "test-model"},
		TaskRegistry: task.NewRegistry(),
		Tools:        []api.Tool{{Name: "read_file"}},
	}

	out, err := loop.executeAgentSpawn(map[string]any{
		"description": "survey repo",
		"prompt":      "look around and report",
	})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	taskID, _ := resp["task_id"].(string)
	if taskID == "" || resp["status"] != "running" {
		t.Fatalf("spawn response wrong: %v", resp)
	}
	if resp["model"] != "test-model" {
		t.Errorf("model must inherit the session model, got %v", resp["model"])
	}

	// Wait for the background run to finish.
	deadline := time.Now().Add(5 * time.Second)
	var final task.Task
	for {
		got, ok := loop.TaskRegistry.Get(taskID)
		if ok && got.Status.IsTerminal() {
			final = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subagent did not finish; last=%+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Status != task.StatusCompleted {
		t.Fatalf("status = %v, want completed", final.Status)
	}
	output, err := loop.TaskRegistry.Output(taskID)
	if err != nil || !strings.Contains(output, "subagent report") {
		t.Errorf("task output must carry the transcript: %q err=%v", output, err)
	}

	// Completion must queue a <system-reminder> notification and flushing
	// must inject it as an IsInjected user message naming the task. The
	// reminder is queued right after the terminal status is written, so wait
	// for the reminder itself, not for the status that precedes it.
	for {
		loop.remindersMu.Lock()
		queued := len(loop.pendingReminders)
		loop.remindersMu.Unlock()
		if queued > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completion reminder never queued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	loop.flushSystemReminders()
	msgs := loop.Session.Messages
	if len(msgs) == 0 {
		t.Fatal("no reminder injected")
	}
	last := msgs[len(msgs)-1]
	text := last.Content[0].Text
	if !last.IsInjected || !strings.Contains(text, taskID) || !strings.Contains(text, "task_output") {
		t.Errorf("completion notification wrong: injected=%v text=%q", last.IsInjected, text)
	}

	// Unknown subagent type is an explicit error.
	if _, err := loop.executeAgentSpawn(map[string]any{
		"description": "x", "prompt": "y", "subagent_type": "ghost",
	}); err == nil {
		t.Error("unknown subagent_type must error")
	}
}

func TestDrainSubagentEventsAutoReplies(t *testing.T) {
	events := make(chan TurnEvent, 8)
	permReply := make(chan PermDecision, 1)
	askReply := make(chan string, 1)

	events <- TurnEvent{Type: TurnEventTextDelta, Text: "hello "}
	events <- TurnEvent{Type: TurnEventPermissionAsk, ToolName: "bash", PermReply: permReply}
	events <- TurnEvent{Type: TurnEventAskUser, AskUserReply: askReply}
	events <- TurnEvent{Type: TurnEventToolStart, ToolName: "read_file", ToolInput: "main.go"}
	events <- TurnEvent{Type: TurnEventTextDelta, Text: "world"}
	close(events)

	var out strings.Builder
	drainSubagentEvents(events, &out)

	if d := <-permReply; d != PermDecisionDeny {
		t.Errorf("background permission ask must be denied, got %v", d)
	}
	if a := <-askReply; !strings.Contains(a, "unattended") {
		t.Errorf("ask_user must get the unattended notice, got %q", a)
	}
	text := out.String()
	if !strings.Contains(text, "hello ") || !strings.Contains(text, "world") || !strings.Contains(text, "[tool: read_file]") {
		t.Errorf("transcript accumulation wrong: %q", text)
	}
}

// Silence the unused-import guard if tools is only used in one test build.
var _ = tools.DefineSubagentTool
