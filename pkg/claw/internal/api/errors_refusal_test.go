package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The gateway's own words, measured on iterion.cloud on 2026-11-25 (LiteLLM in
// front of vLLM), issue #3048.
const (
	litellmBudgetBody      = `{"error":{"message":"Budget has been exceeded! Current cost: 150.18, Max budget: 150.0","type":"budget_exceeded","param":null,"code":"422"}}`
	litellmBudgetMessage   = "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"
	litellmMultimodalMsg   = "litellm.BadRequestError: OpenAIException - glm-5.3 is not a multimodal model"
	litellmGrammarRegexMsg = "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty:  (And (SearchRegex /^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$/) (Regex /(?s:.{1,253})/))"
)

func TestClassifyRefusalReadsTheRefusalBeforeTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		message, body string
		want          RefusalKind
	}{
		{"budget as 422 (run 01a211c3)", 422, litellmBudgetMessage, litellmBudgetBody, RefusalBudget},
		{"budget as 400", 400, litellmBudgetMessage, "", RefusalBudget},
		{"budget as 429", 429, "", litellmBudgetBody, RefusalBudget},
		{"budget as 500", 500, litellmBudgetMessage, "", RefusalBudget},
		{"litellm key budget", 400, "Authentication Error - ExceededTokenBudget: Current spend for token: 10.2; Max Budget for Token: 10.0", "", RefusalBudget},
		{"anthropic credit", 400, "Your credit balance is too low to access the Anthropic API.", "", RefusalBudget},
		{"402 payment required", 402, "payment required", "", RefusalBudget},
		{"not multimodal (run 01a211cd-79f7)", 400, litellmMultimodalMsg, "", RefusalCapability},
		{"grammar as 500 (run 01a211cd-78df)", 500, litellmGrammarRegexMsg, "", RefusalGrammar},
		{"422 request", 422, "Unprocessable Entity", "", RefusalRequest},
		{"415 request", 415, "unsupported media type", "", RefusalRequest},
		{"bare 400 stays the sample's", 400, "litellm.BadRequestError: content_filter", "", RefusalNone},
		{"401", 401, "invalid api key", "", RefusalNone},
		{"403", 403, "forbidden", "", RefusalNone},
		{"404", 404, "model not found", "", RefusalNone},
		{"408", 408, "timeout", "", RefusalNone},
		{"409", 409, "conflict", "", RefusalNone},
		{"413", 413, "request too large", "", RefusalNone},
		{"429", 429, "rate limited", "", RefusalNone},
		{"503", 503, "upstream connect error", "", RefusalNone},
		{"transport", 0, "dial tcp: connection refused", "", RefusalNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyRefusal(tc.status, tc.message, tc.body); got != tc.want {
				t.Errorf("ClassifyRefusal(%d, %q, %q) = %q, want %q", tc.status, tc.message, tc.body, got, tc.want)
			}
			e := NewHTTPError("openai", tc.status, tc.message, tc.body)
			if tc.want != RefusalNone && (e.Retryable || e.IsRetryable()) {
				t.Errorf("a %s refusal is retryable: %+v", tc.want, e)
			}
		})
	}
}

// IsRetryable is the verdict, whatever built the error: a literal a provider
// built from the status alone, or one relayed across the sandbox runner with
// its stored flag set, still refuses a refusal.
func TestIsRetryableRefusesARefusalWhateverBuiltIt(t *testing.T) {
	for _, e := range []*APIError{
		{Provider: "anthropic", StatusCode: 500, Message: litellmBudgetMessage, Retryable: true},
		{Provider: "openai", StatusCode: 500, Message: litellmGrammarRegexMsg, Retryable: true},
		{Provider: "openai", StatusCode: 503, Body: litellmBudgetBody, Retryable: true},
	} {
		if e.IsRetryable() {
			t.Errorf("%v: IsRetryable = true for a %s refusal", e, e.Refusal())
		}
	}
	if !(&APIError{StatusCode: 503, Message: "upstream connect error", Retryable: true}).IsRetryable() {
		t.Error("a plain 503 is no longer retryable")
	}
}

func TestRefusalOfReadsTheTypedErrorThroughWrappers(t *testing.T) {
	wrapped := fmt.Errorf("claw backend: runner: %w", NewHTTPError("openai", 422, litellmBudgetMessage, litellmBudgetBody))
	if got := RefusalOf(wrapped); got != RefusalBudget {
		t.Errorf("RefusalOf(wrapped budget) = %q", got)
	}
	if got := RefusalOf(errors.New(litellmBudgetMessage)); got != RefusalNone {
		t.Errorf("RefusalOf(flattened text) = %q — a refusal is read from its type, never its text", got)
	}
}

// The Anthropic-wire client has its own retry loop: a refusal served as a 5xx
// is one call, not defaultMaxRetries.
func TestAnthropicClientDoesNotRetryARefusalServedAs5xx(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(litellmBudgetBody))
	}))
	defer ts.Close()
	client := NewClient("sk-test", "claude-test")
	client.BaseURL = ts.URL
	_, err := client.StreamResponse(context.Background(), CreateMessageRequest{
		Model:     "claude-test",
		MaxTokens: 100,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Refusal() != RefusalBudget || apiErr.IsRetryable() {
		t.Fatalf("err = %v, want a non-retryable budget refusal", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d calls for a budget refusal, want 1", n)
	}
}
