package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The ChatGPT-Codex backend refuses a model to a client it deems too old
// with an HTTP 400 (measured 2026-09-07); Anthropic refuses an unknown model
// id with a 404 not_found_error. Both are deterministic for the credential
// and must surface as the engine's typed ErrModelUnavailable, with the
// original APIError still reachable.
func TestWithModelRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  *api.APIError
	}{
		{"chatgpt-codex client too old", &api.APIError{Provider: "openai", StatusCode: 400, Message: `{"detail":"The 'gpt-6-astra' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."}`}},
		{"openai model_not_found", &api.APIError{Provider: "openai", StatusCode: 404, Message: `{"error":{"message":"The model 'gpt-99' does not exist or you do not have access to it.","type":"invalid_request_error","code":"model_not_found"}}`}},
		{"anthropic not_found_error", &api.APIError{Provider: "anthropic", StatusCode: 404, Message: `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wrapped := fmt.Errorf("claw backend: structured generation: %w", c.err)
			got := withModelRefusal(wrapped, "some/model")
			var unavailable *delegate.ErrModelUnavailable
			if !errors.As(got, &unavailable) {
				t.Fatalf("got %T (%v), want *delegate.ErrModelUnavailable", got, got)
			}
			if unavailable.Model != "some/model" || unavailable.Provider != "claw/"+c.err.Provider || unavailable.Detail != c.err.Message {
				t.Errorf("typed error = %+v", unavailable)
			}
			var apiErr *api.APIError
			if !errors.As(got, &apiErr) || apiErr != c.err {
				t.Errorf("the original APIError must stay reachable through Cause: %v", got)
			}
		})
	}
}

func TestWithModelRefusal_LeavesOtherErrorsAlone(t *testing.T) {
	for _, e := range []*api.APIError{
		{Provider: "openai", StatusCode: 429, Message: "rate limited"},
		{Provider: "openai", StatusCode: 400, Message: `{"error":{"message":"messages must not be empty"}}`},
		{Provider: "openai", StatusCode: 500, Message: "model not found"},
		{Provider: "openai", StatusCode: 401, Message: "invalid model key"},
	} {
		err := fmt.Errorf("x: %w", e)
		if got := withModelRefusal(err, "m"); got != err {
			t.Errorf("%d %q: got %v, want the error unchanged", e.StatusCode, e.Message, got)
		}
	}
	typed := &delegate.ErrModelUnavailable{Model: "m"}
	if got := withModelRefusal(typed, "m"); got != typed {
		t.Errorf("an already typed error must pass through, got %v", got)
	}
	plain := errors.New("boom")
	if got := withModelRefusal(plain, "m"); got != plain {
		t.Errorf("a non-API error must pass through, got %v", got)
	}
}
