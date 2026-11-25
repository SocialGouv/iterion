package api

import "testing"

// The refusals below are the gateway's own words (PLIAGE: LiteLLM in front
// of vLLM), collected from production runs and direct probes.
var grammarRefusals = map[string]string{
	"xgrammar":        "litellm.BadRequestError: OpenAIException - The provided JSON schema contains features not supported by xgrammar.",
	"unimplemented":   `litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unimplemented keys: ["propertyNames"]`,
	"regex emptiness": "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty:  (And (SearchRegex /^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$/) (Regex /(?s:.{1,253})/))",
}

func TestAPIErrorIsGrammarRefusal(t *testing.T) {
	for name, msg := range grammarRefusals {
		t.Run(name, func(t *testing.T) {
			if !(&APIError{StatusCode: 500, Message: msg}).IsGrammarRefusal() {
				t.Errorf("message %q: not read as a grammar refusal", msg)
			}
			if !(&APIError{StatusCode: 500, Body: msg}).IsGrammarRefusal() {
				t.Errorf("body %q: not read as a grammar refusal", msg)
			}
		})
	}
	for _, msg := range []string{
		"litellm.APIConnectionError: upstream connect error or disconnect/reset before headers",
		"Budget has been exceeded! Current cost: 150.18, Max budget: 150.0",
		"glm-5.3 is not a multimodal model",
	} {
		if (&APIError{StatusCode: 500, Message: msg}).IsGrammarRefusal() {
			t.Errorf("%q read as a grammar refusal", msg)
		}
	}
	var nilErr *APIError
	if nilErr.IsGrammarRefusal() {
		t.Error("a nil APIError is not a grammar refusal")
	}
}

func TestNewHTTPErrorNeverRetriesAGrammarRefusal(t *testing.T) {
	for _, tc := range []struct {
		status    int
		message   string
		retryable bool
	}{
		{500, grammarRefusals["regex emptiness"], false},
		{500, grammarRefusals["unimplemented"], false},
		{400, grammarRefusals["xgrammar"], false},
		{500, "upstream connect error", true},
		{429, "rate limited", true},
		{400, "bad request", false},
	} {
		got := NewHTTPError("openai", tc.status, tc.message, "")
		if got.Retryable != tc.retryable || got.IsRetryable() != tc.retryable {
			t.Errorf("%d %q: Retryable = %v, want %v", tc.status, tc.message, got.Retryable, tc.retryable)
		}
		if got.Provider != "openai" || got.StatusCode != tc.status || got.Message != tc.message {
			t.Errorf("fields not carried: %+v", got)
		}
	}
	if NewHTTPError("openai", 500, "opaque", grammarRefusals["xgrammar"]).Retryable {
		t.Error("a refusal named only in the body must not be retryable")
	}
}
