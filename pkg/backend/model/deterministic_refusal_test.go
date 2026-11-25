package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// #3048: a deterministic provider refusal is never retried in the node, by
// the claw generation loop (isRetryable) nor by the delegate retry
// (isDelegateRetryable) — whatever status, stored flag or wording carries it.
func TestDeterministicRefusalIsNeverRetriedInTheNode(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		// LiteLLM serves this one as a 500 whose class name reads as an
		// internal server error to the network-signature list.
		"grammar 500": &api.APIError{Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "litellm.InternalServerError: internal server error - Grammar error: Unable to determine if regex is empty"},
		"budget 422": &api.APIError{Provider: "openai", StatusCode: 422,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		"budget 503": &api.APIError{Provider: "openai", StatusCode: 503, Retryable: true,
			Message: "service unavailable: Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		"multimodal 400": &api.APIError{Provider: "openai", StatusCode: 400,
			Message: "litellm.BadRequestError: OpenAIException - glm-5.3 is not a multimodal model"},
		// A stream error relayed from the sandboxed runner keeps the flag its
		// decoder gave it; the refusal still decides.
		"stream grammar": &APIError{StatusCode: 500, IsRetryable: true,
			Message: "stream error: internal server error: Grammar error: Unimplemented keys: [\"propertyNames\"]"},
	} {
		wrapped := fmt.Errorf("claw backend: runner: %w", err)
		if isRetryable(wrapped) {
			t.Errorf("%s: isRetryable = true", name)
		}
		if isDelegateRetryable(wrapped) {
			t.Errorf("%s: isDelegateRetryable = true", name)
		}
	}
	if !isRetryable(badGateway) || !isDelegateRetryable(fmt.Errorf("claw backend: %w", badGateway)) {
		t.Error("a plain 502 is no longer retried")
	}
}

// Defect B of #3048: a claw agent with tools and a schema whose tool loop
// ends without text and whose recovery pass fails. The node's error must
// carry the recovery's TYPED cause — a %v made the gateway's grammar refusal
// plain text, the node failed EXECUTION_FAILED, and the whole tool loop was
// replayed blind.
func TestEmptyToolLoopKeepsTheRecoveryPassTypedCause(t *testing.T) {
	t.Parallel()
	grammar := &api.APIError{Provider: "openai", StatusCode: 500, Retryable: true,
		Message: "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty"}
	client := &funcClient{fn: func(n int, _ api.CreateMessageRequest) ([]api.StreamEvent, error) {
		if n == 1 {
			return textEvents("", 100, 1), nil // the tool loop ends without text
		}
		return nil, grammar // every recovery call meets the refusal
	}}
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	_, err := b.generateTextWithToolsAndSchema(context.Background(), client,
		delegate.Task{NodeID: "judge", Model: "openai_compatible/scaleway/glm-test", OutputSchema: json.RawMessage(recoverySchema)}, recoveryRetryOpts())
	if err == nil {
		t.Fatal("a node with no answer succeeded")
	}
	var typed *api.APIError
	if !errors.As(err, &typed) || typed != grammar {
		t.Fatalf("the recovery pass's typed cause is lost: %v", err)
	}
	if got := api.RefusalOf(err); got != api.RefusalGrammar {
		t.Errorf("RefusalOf = %q, want grammar", got)
	}
	if isRetryable(err) || isDelegateRetryable(err) {
		t.Error("the refused node is retried in place")
	}
}
