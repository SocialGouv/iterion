package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The declared schema is read BEFORE any request is built, so a schema the
// API types cannot represent costs a run every attempt and never a token.
// It must leave here wearing a type the classifier can read, not a
// sentence — measured on run 01a07db7, where the sentence was classified
// EXECUTION_FAILED and redelivered onto four pods.
func TestGenerateObjectDirect_AnUnreadableSchemaIsTyped(t *testing.T) {
	// Unreadable under ANY definition of the schema types — deliberately
	// not the `type` union that produced the measured failure, because
	// that one becomes readable the day the backend's types accept it,
	// and this test would then prove nothing while still passing.
	schema := json.RawMessage(`{"type":"object","properties":"not-an-object"}`)

	_, err := GenerateObjectDirect[map[string]any](context.Background(), nil, GenerationOptions{
		ExplicitSchema: schema,
		Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("a schema whose `properties` is a string was accepted — the guard this test pins is gone")
	}
	var unusable *delegate.ErrSchemaUnusable
	if !errors.As(err, &unusable) {
		t.Fatalf("error is not *delegate.ErrSchemaUnusable, so the classifier reads a sentence: %v", err)
	}
	if unusable.Detail == "" {
		t.Error("Detail empty — the diagnosis that names the offending field is lost")
	}
}
