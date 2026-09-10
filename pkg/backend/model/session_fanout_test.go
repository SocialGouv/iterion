package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// sessionEchoBackend stands in for the claw backend at the one seam that
// matters here: it runs the REAL applySessionMessages / captureSessionMessages
// around a faked generation, so what it records is the message list a live
// model would have been sent. failFirst makes attempt 1 fail AFTER it has
// produced a transcript — the shape that leaves a session behind, since the
// executor evicts on success only.
type sessionEchoBackend struct {
	calls [][]api.Message // the merged messages each call would have sent
	n     int
}

func (b *sessionEchoBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	b.n++
	mine := api.Message{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: "item-" + itoa(b.n)}},
	}
	opts := applySessionMessages(ctx, task.NodeID, GenerationOptions{Messages: []api.Message{mine}})
	b.calls = append(b.calls, opts.Messages)
	// The generation succeeded and accumulated a transcript, whatever the
	// node then does with it.
	captureSessionMessages(ctx, task.NodeID, &TextResult{Messages: opts.Messages})
	if b.n == 1 {
		return delegate.Result{}, errors.New("item 1 blew up after producing a transcript")
	}
	return delegate.Result{Output: map[string]any{"text": "ok"}, BackendName: delegate.BackendClaw}, nil
}

func itoa(n int) string { return string(rune('0' + n)) }

// TestPerNodeSessionFollowsTheRunIdentity pins the contract the engine's
// branch dispatch relies on: the per-node session store is reachable ONLY
// through the ctx run identity, and its key is `(runID, nodeID)` with no
// branch discriminator.
//
// So the identity decides which of two behaviours a repeated node id gets,
// and both are intended:
//
//   - WITH it (the trunk, where a node id runs once per run): a failed
//     attempt's transcript is replayed to the next one. That replay is why
//     the store exists — CompactAndRetry shrinks it and the model resumes.
//   - WITHOUT it (a fan-out branch, where `fan_out_each` replays ONE node id
//     per item): nothing is stored and nothing is replayed, so item 2 cannot
//     inherit item 1's conversation.
//
// If the gate below ever stops honouring an empty run id, fan-out items
// start sharing one session slot and this test is what says so.
func TestPerNodeSessionFollowsTheRunIdentity(t *testing.T) {
	tests := []struct {
		name        string
		withRunID   bool
		wantReplay  bool
		wantSecond  int // messages the second execution would have sent
		explanation string
	}{
		{
			name:        "trunk identity replays the failed attempt",
			withRunID:   true,
			wantReplay:  true,
			wantSecond:  2,
			explanation: "the recovery path lost the transcript it compacts",
		},
		{
			name:        "fan-out branch (no identity) stays isolated",
			withRunID:   false,
			wantReplay:  false,
			wantSecond:  1,
			explanation: "a sibling item's conversation leaked into this one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &sessionEchoBackend{}
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaw, backend)
			exec := NewClawExecutor(NewRegistry(), &ir.Workflow{},
				WithBackendRegistry(reg),
				// One backend call per Execute: this test is about what
				// crosses an EXECUTION boundary, not in-executor retry.
				WithRetryPolicy(RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}),
			)

			// One node id, executed twice — what a `fan_out_each` over two
			// items does, and what the trunk does across a retry.
			node := &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "per_item"},
				LLMFields: ir.LLMFields{Backend: delegate.BackendClaw, Model: "stub-model"},
			}

			ctx := context.Background()
			if tt.withRunID {
				ctx = WithRunID(ctx, "run-1")
			}
			if _, err := exec.Execute(ctx, node, nil); err == nil {
				t.Fatal("first execution should have failed — it is what leaves a session behind")
			}
			if _, err := exec.Execute(ctx, node, nil); err != nil {
				t.Fatalf("second execution: %v", err)
			}

			if len(backend.calls) != 2 {
				t.Fatalf("backend saw %d calls, want 2", len(backend.calls))
			}
			second := backend.calls[1]
			if len(second) != tt.wantSecond {
				t.Fatalf("second execution sent %d messages, want %d — %s (messages: %v)",
					len(second), tt.wantSecond, tt.explanation, second)
			}
			replayed := len(second) > 1
			if replayed != tt.wantReplay {
				t.Fatalf("replay = %v, want %v", replayed, tt.wantReplay)
			}
			if !tt.wantReplay {
				return
			}
			if got := second[0].Content[0].Text; got != "item-1" {
				t.Errorf("replayed head = %q, want the first attempt's own message", got)
			}
		})
	}
}
