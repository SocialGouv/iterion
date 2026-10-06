package model

import (
	"context"
	"errors"
	api "github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The adaptive-routing ladder's same-backend rung (ADR-121 §2, the F1
// arbitration): a fall-through that KEEPS the harness KEEPS the session —
// the store is keyed by slot/node, not by the credential serving it, and
// the stage answers with its own vendor (GLM via zai on a claude session).
// A CROSS-backend fall-through still starts fresh (the conversation belongs
// to the element that paused).
func TestElementBuilder_SameBackendFallThroughKeepsTheSession(t *testing.T) {
	registry := NewRegistry()
	registry.Register("codex", func(modelID string) (api.APIClient, error) {
		return nil, errors.New("not dispatched in this test")
	})
	e := NewClawExecutor(registry, &ir.Workflow{})
	base := &stubBackend{}
	assemble := func(ctx context.Context, backendName string) (*delegate.Task, error) {
		return &delegate.Task{
			SessionID:    "sess-1",
			SessionSlot:  "slot",
			Model:        "claude-opus-5-5",
			ProviderHint: "",
		}, nil
	}
	eb := e.newElementBuilder("n1", "claude_code", base, assemble)
	ctx := context.Background()

	sameBackendElement := chainElement{Backend: "claude_code", Model: "claude-opus-5-5"}
	_, _, taskSame, err := eb(ctx, 1, sameBackendElement, "")
	if err != nil {
		t.Fatalf("same-backend build: %v", err)
	}
	if taskSame.SessionID != "sess-1" {
		t.Fatalf("same-backend fall-through SessionID = %q, want carried — GLM answering a claude id keeps the session", taskSame.SessionID)
	}
	// The cross-backend drop is the pre-existing default (the block this
	// extends only ADDS a keep): its fixture needs a delegate registry
	// serving the second harness — deliberately not built here.
}
