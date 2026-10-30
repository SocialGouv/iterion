package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// dispatchBound is how long a witness waits for a sandbox dispatch it drove
// to return. It bounds a HANG, not a slow machine: a dispatch here spawns a
// process, and the seed of a 40 000-turn session runs the whole compaction
// pass the channel's bound needs — 3.6 s unexercised by the race detector,
// past a minute under `go test -race` on a loaded box, where 60 s failed the
// witness of a refusal that was working. What it has to catch is the abort
// that never returns, which no margin makes finite.
const dispatchBound = 300 * time.Second

// A session slot is ONE conversation two nodes share. Across the sandbox
// boundary the host holds it: the mirror of the runner's capture writes it,
// the next crossing's pre-spawn seed reads it. Both address it by the task's
// session key, so what the first node said is what the second node's runner
// replays — and nothing is left under the bare node id, a key no other site
// writes and the one a schema re-ask can mistake for the answer it is
// re-asking about.
func TestSandboxSessionMirrorAndSeedShareTheTasksSessionKey(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	for _, tc := range []struct {
		name  string
		slot  string
		first string
		next  string
	}{
		{name: "an explicit slot, two nodes", slot: "assistant_conversation", first: "draft", next: "revise"},
		{name: "no slot: the node id is the key", slot: "", first: "copi", next: "copi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newNodeSessionStore()
			ctx := withRuntimeContext(context.Background(), runID, store)
			b := newTestClawBackend()

			first := delegate.Task{NodeID: tc.first, SessionSlot: tc.slot, Model: "openai/gpt-5"}
			snapshot := mustMarshalMessages(t, []api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: "the-first-nodes-turn"}},
			}})
			handler, _ := b.multiplexerHandler(ctx, first, sessionSeed{})
			handler.OnSessionCapture(snapshot)

			if got := store.load(runID, taskSessionKey(first)); len(got) == 0 {
				t.Fatalf("the runner's capture did not land under the task's session key %q — the slot's conversation never accumulates on the host", taskSessionKey(first))
			}
			if tc.slot != "" {
				if got := store.load(runID, tc.first); len(got) != 0 {
					t.Errorf("the mirror also wrote the bare node id %q: an entry no other site writes, which a schema re-ask can serve as the answer it is re-asking about", tc.first)
				}
			}

			// The next crossing on the same slot: the pre-spawn seed must
			// send that conversation ahead of the task envelope.
			next := delegate.Task{NodeID: tc.next, SessionSlot: tc.slot, Model: "openai/gpt-5", WorkDir: t.TempDir()}
			env := firstDispatchedEnvelope(t, b, ctx, next)
			if env.Type != delegate.EnvelopeSessionReplay {
				t.Fatalf("the runner's first envelope is %q, want %q — the next crossing is spawned with none of the slot's conversation", env.Type, delegate.EnvelopeSessionReplay)
			}
			if !strings.Contains(string(env.Data), "the-first-nodes-turn") {
				t.Fatalf("the session_replay envelope does not carry the captured turn: %s", env.Data)
			}
		})
	}
}

// A schema re-ask replays the conversation the backend captured under the
// task's session key. For a slot node the bare node id is not that key and
// nothing writes it: anything found there is residue from an earlier
// crossing, never the answer being re-asked about.
func TestCompletedClawConversation_ReadsOnlyTheTasksSessionKey(t *testing.T) {
	t.Parallel()
	const runID = "run-1"
	stale := []api.Message{{
		Role:    "assistant",
		Content: []api.ContentBlock{{Type: "text", Text: "an-earlier-crossing"}},
	}}
	slotted := []api.Message{{
		Role:    "assistant",
		Content: []api.ContentBlock{{Type: "text", Text: "this-nodes-answer"}},
	}}

	t.Run("residue under the bare node id is not the answer", func(t *testing.T) {
		store := newNodeSessionStore()
		store.save(runID, "copi", stale)
		ctx := withRuntimeContext(context.Background(), runID, store)
		e := NewClawExecutor(NewRegistry(), &ir.Workflow{})
		task := &delegate.Task{NodeID: "copi", SessionSlot: "assistant_conversation"}
		if got := e.completedClawConversation(ctx, task, delegate.Result{}); got != nil {
			t.Fatalf("the re-ask replayed %s — a transcript stored under the bare node id, not this slot's conversation", got)
		}
	})

	t.Run("the slot's own conversation is the answer", func(t *testing.T) {
		store := newNodeSessionStore()
		store.save(runID, "assistant_conversation", slotted)
		ctx := withRuntimeContext(context.Background(), runID, store)
		e := NewClawExecutor(NewRegistry(), &ir.Workflow{})
		task := &delegate.Task{NodeID: "copi", SessionSlot: "assistant_conversation"}
		got := e.completedClawConversation(ctx, task, delegate.Result{})
		if !strings.Contains(string(got), "this-nodes-answer") {
			t.Fatalf("the re-ask replayed %s, want the slot's captured conversation", got)
		}
	})
}

// CompactAndRetry shrinks the conversation the next attempt replays. The
// store holds that conversation under the node's session key, so Compact —
// handed only a node id by the recovery dispatcher — resolves the key from
// the workflow. Keyed by the bare node id it finds nothing for a slot node,
// reports "nothing to compact", and the retry replays the whole
// pre-overflow history again.
func TestClawExecutorCompact_FindsASlotNodesConversation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		node ir.Node
		key  string
	}{
		{
			name: "a persisted node with an explicit slot",
			node: &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "copi"},
				LLMFields: ir.LLMFields{Backend: delegate.BackendClaw, Model: "openai/gpt-5"},
				Session:   ir.SessionPersist, SessionSlot: "assistant_conversation",
			},
			key: "assistant_conversation",
		},
		{
			name: "a node without a slot",
			node: &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "copi"},
				LLMFields: ir.LLMFields{Backend: delegate.BackendClaw, Model: "openai/gpt-5"},
			},
			key: "copi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewClawExecutor(NewRegistry(), &ir.Workflow{Nodes: map[string]ir.Node{"copi": tc.node}})
			msgs := make([]api.Message, 0, 20)
			for range 20 {
				msgs = append(msgs, api.Message{
					Role:    "user",
					Content: []api.ContentBlock{{Type: "text", Text: largePadding()}},
				})
			}
			e.sessions.save("run-1", tc.key, msgs)

			if err := e.Compact(WithRunID(context.Background(), "run-1"), "copi"); err != nil {
				t.Fatalf("Compact: %v — the recovery path found no conversation to shrink under %q", err, tc.key)
			}
			if got := e.sessions.load("run-1", tc.key); len(got) >= 20 {
				t.Fatalf("the conversation under %q still holds %d messages — the retry replays the whole pre-overflow history", tc.key, len(got))
			}
		})
	}
}

// firstDispatchedEnvelope sends one task through b's real sandbox dispatch to
// a runner that keeps the first envelope and never answers, and returns that
// envelope.
//
// The dispatch is driven on its own goroutine and WAITED FOR under a bound:
// the launcher's abort path (Kill then Wait) does not always return — a pipe
// the runner still holds keeps Wait blocked — so a dispatch that strands must
// redden the witness that drove it, on its own assertion, instead of hanging
// the package until the 10-minute test timeout takes every other test in it
// down with no indication of which one stalled.
func firstDispatchedEnvelope(t *testing.T, b *ClawBackend, ctx context.Context, task delegate.Task) delegate.Envelope {
	t.Helper()
	captured := filepath.Join(t.TempDir(), "first.ndjson")
	task.Sandbox = &taskCaptureSandbox{path: captured}
	done := make(chan error, 1)
	go func() {
		_, err := b.executeViaSandboxRunner(ctx, task)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a runner that never answers must fail the dispatch")
		}
	case <-time.After(dispatchBound):
		t.Fatalf("the dispatch did not return in %s — it is stranded in its abort, so the node neither answers nor fails", dispatchBound)
	}
	line, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read what the runner received: %v", err)
	}
	var env delegate.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("decode the runner's first envelope %q: %v", line, err)
	}
	return env
}

func mustMarshalMessages(t *testing.T, msgs []api.Message) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return raw
}

// The recovery dispatcher hands Compact a node id and reports back what it
// found. An empty store is not a failure, but it is not a compaction
// either: the run log already says compact_and_retry was chosen, so the
// sentinel names the ADDRESS that held nothing — for a slot node that is
// the slot, not the node id the dispatcher knows it by.
func TestClawExecutorCompact_NamesTheKeyItFoundEmpty(t *testing.T) {
	t.Parallel()
	e := NewClawExecutor(NewRegistry(), &ir.Workflow{Nodes: map[string]ir.Node{"copi": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "copi"},
		LLMFields: ir.LLMFields{Backend: delegate.BackendClaw, Model: "openai/gpt-5"},
		Session:   ir.SessionPersist, SessionSlot: "assistant_conversation",
	}}})
	err := e.Compact(WithRunID(context.Background(), "run-1"), "copi")
	if !errors.Is(err, ErrCompactionUnsupported) {
		t.Fatalf("Compact over an empty store = %v, want ErrCompactionUnsupported", err)
	}
	if !strings.Contains(err.Error(), `"assistant_conversation"`) {
		t.Errorf("the reported gap does not name the key it found empty: %v — the operator cannot tell which address was consulted", err)
	}
}
