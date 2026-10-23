package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// #2710: a long tool loop's structured recovery on a gateway model can come
// back empty at a high reasoning effort — four identical deaths on the vao
// pool, ~$5 of diff reading lost per death. The recovery now runs once more
// at a lowered effort before the node fails: one billed call against a full
// restart of the review.

func recoveryRetryOpts() GenerationOptions {
	return GenerationOptions{
		Model:               "openai_compatible/scaleway/glm-test",
		Messages:            []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Judge the diff."}}}},
		ProviderOptions:     map[string]any{"reasoning_effort": "max"},
		ExplicitSchema:      json.RawMessage(recoverySchema),
		MaxSteps:            5,
		AnswerFromReasoning: true,
	}
}

func TestClawBackend_RecoveryRetriesAtALoweredEffort(t *testing.T) {
	var wireEfforts []string
	client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
		wireEfforts = append(wireEfforts, req.ReasoningEffort)
		switch n {
		case 1, 2, 3: // the loop's turn, the recovery's forced turn and its own unforced re-ask: empty
			return textEvents("", 100, 1), nil
		case 4: // the lowered retry: the model commits through the schema tool
			return toolUseEvents("so_1", "structured_output",
				`{"batch_id":"b1","notes":"real findings from the diff"}`, 100, 20), nil
		}
		return nil, badGateway
	}}
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	res, err := b.generateTextWithToolsAndSchema(context.Background(), client,
		delegate.Task{NodeID: "judge", Model: "openai_compatible/scaleway/glm-test", OutputSchema: json.RawMessage(recoverySchema)}, recoveryRetryOpts())
	if err != nil {
		t.Fatalf("the lowered retry did not save the review: %v", err)
	}
	if got, _ := res.Output["notes"].(string); got != "real findings from the diff" {
		t.Errorf("notes = %q, want the retry's structured answer", got)
	}
	if !res.FormattingPassUsed {
		t.Errorf("FormattingPassUsed = false — the retry IS a formatting pass")
	}
	if client.count() != 4 {
		t.Errorf("%d provider calls, want the loop(1) + the recovery's two turns(2) + the lowered retry(1)", client.count())
	}
	// The fix's core mechanism ON THE WIRE: the retry's request carries the
	// lowered effort, not the same max request that came back empty.
	if len(wireEfforts) != 4 || wireEfforts[0] != "max" || wireEfforts[1] != "max" ||
		wireEfforts[2] != "max" || wireEfforts[3] != "low" {
		t.Errorf("wire efforts = %v, want max,max,max,low", wireEfforts)
	}
}

func TestClawBackend_TheRetryIsBilledIntoTheFailure(t *testing.T) {
	client := &funcClient{fn: func(n int, _ api.CreateMessageRequest) ([]api.StreamEvent, error) {
		return textEvents("", 100, 1), nil // every turn comes back empty
	}}
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	_, err := b.generateTextWithToolsAndSchema(context.Background(), client,
		delegate.Task{NodeID: "judge", Model: "openai_compatible/scaleway/glm-test", OutputSchema: json.RawMessage(recoverySchema)}, recoveryRetryOpts())
	if err == nil {
		t.Fatal("a node with nothing on any turn succeeded")
	}
	if !strings.Contains(err.Error(), "empty response after tool loop") {
		t.Errorf("the error lost the original cause: %v", err)
	}
	if client.count() != 5 {
		t.Errorf("%d provider calls, want the loop(1) + the recovery's two turns(2) + the lowered retry's two(2)", client.count())
	}
}
