package model

import (
	"errors"
	"strings"
	"testing"
)

// An error event from claw's OpenAI provider is classified on the
// provider's own verdict: a chat-wire error frame or a failed Responses API
// response — a failure the provider hit after accepting the request — is
// retried unless its type or code names a condition no new request clears.
// A code a JSON error frame is classified by keeps that classification
// (context overflow, quota), and every other verdict is a typed *APIError,
// which isRetryable reads as is.
func TestClassifyStreamEventError_OpenAIProviderCodes(t *testing.T) {
	cases := []struct {
		name      string
		msg       string
		retryable bool
		overflow  bool
		contains  string
	}{
		// retried
		{"chat server error", "openai stream error: The server had an error while processing your request (type=server_error)", true, false, ""},
		{"chat rate limit", "openai stream error: Rate limit reached (type=rate_limit_error, code=rate_limit_exceeded)", true, false, ""},
		{"chat uppercase rate limit", "openai stream error: Slow down (type=RATE_LIMIT_EXCEEDED, code=RATE_LIMIT_EXCEEDED)", true, false, ""},
		{"chat 5xx code", "openai stream error: upstream unavailable (code=503)", true, false, ""},
		{"chat 408 code", "openai stream error: Request timed out (code=408)", true, false, ""},
		{"chat 409 code", "openai stream error: Conflict (code=409)", true, false, ""},
		{"LiteLLM timeout labelled invalid_request_error", "openai stream error: litellm.Timeout: Request timed out (type=invalid_request_error, code=408)", true, false, ""},
		{"LiteLLM conflict labelled invalid_request_error", "openai stream error: Conflict (type=invalid_request_error, code=409)", true, false, ""},
		{"chat vLLM mid-stream catch-all", "openai stream error: Background loop has errored already. (type=BadRequestError, code=400)", true, false, ""},
		{"chat frame naming no type or code", "openai stream error: Request failed during generation: worker died", true, false, ""},
		{"chat Python exception head", "openai stream error: RuntimeError: CUDA error: device-side assert triggered", true, false, ""},
		{"chat LiteLLM exception head, suffix cut away", "openai stream error: litellm.APIConnectionError: peer closed connection without sending complete message body", true, false, ""},
		{"chat exception head naming a rate limit", "openai stream error: RateLimitError: Too many requests", true, false, ""},
		{"chat code-only raw JSON", "openai stream error: {\"code\":503}", true, false, ""},
		{"chat nested raw JSON", "openai stream error: {\"error\":{\"message\":\"Overloaded\",\"type\":\"overloaded_error\"}}", true, false, ""},
		{"chat cut data line", "openai stream error: unparseable data frame: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel", true, false, ""},
		{"responses server error", "openai stream error: server_error: The server had an error", true, false, ""},
		{"responses uppercase server error", "openai stream error: SERVER_ERROR: boom", true, false, ""},
		{"responses failed rate limit", "openai response failed: rate_limit_exceeded: slow down", true, false, ""},
		{"responses forfait slow_down", "openai response failed: slow_down: slow down", true, false, ""},
		{"responses failed with an unknown code", "openai response failed: unknown_code: something new", true, false, ""},
		{"responses failure naming its code alone", "openai response failed: server_is_overloaded", true, false, ""},
		{"responses failed with a nested error emptied", "openai stream error: {\"error\":{},\"code\":503}", true, false, ""},
		{"bare responses error", "openai stream error", true, false, ""},
		{"bare responses failure", "openai response failed", true, false, ""},

		// permanent
		{"chat context overflow", "openai stream error: Input too long (type=invalid_request_error, code=context_length_exceeded)", false, true, ""},
		{"chat quota", "openai stream error: You exceeded your current quota (type=insufficient_quota, code=insufficient_quota)", false, false, "Quota exceeded"},
		{"chat invalid request", "openai stream error: Invalid value for tool_choice (type=invalid_request_error)", false, false, ""},
		{"chat invalid request with a 400", "openai stream error: Bad (type=invalid_request_error, code=400)", false, false, ""},
		{"chat uppercase permanent type", "openai stream error: Bad request (type=INVALID_REQUEST_ERROR)", false, false, ""},
		{"chat authentication type alone", "openai stream error: Who are you (type=authentication_error)", false, false, ""},
		{"chat permission type", "openai stream error: Not allowed (type=permission_error, code=x)", false, false, ""},
		{"chat not-found type", "openai stream error: No such thing (type=not_found_error, code=x)", false, false, ""},
		{"chat billing type", "openai stream error: Pay first (type=billing_error, code=x)", false, false, ""},
		{"chat quota type alone", "openai stream error: Over quota (type=insufficient_quota)", false, false, ""},
		{"chat content filter under a neutral type", "openai stream error: Blocked (type=server_error, code=content_filter)", false, false, ""},
		{"chat bad key under a neutral type", "openai stream error: Bad key (type=server_error, code=invalid_api_key)", false, false, ""},
		{"chat unknown model under a neutral type", "openai stream error: No model (type=server_error, code=model_not_found)", false, false, ""},
		{"chat uppercase permanent code", "openai stream error: No model (type=api_error, code=MODEL_NOT_FOUND)", false, false, ""},
		{"chat unusable image code", "openai stream error: Bad image (code=invalid_image)", false, false, ""},
		{"vLLM label beside another 4xx", "openai stream error: Unauthorized (type=BadRequestError, code=401)", false, false, ""},
		{"vLLM label beside a permanent code", "openai stream error: Blocked (type=BadRequestError, code=content_filter)", false, false, ""},
		{"chat nested permanent frame", "openai stream error: {\"error\":{\"message\":\"nope\",\"type\":\"permission_error\"}}", false, false, ""},
		{"chat nested uppercase type", "openai stream error: {\"error\":{\"type\":\"PERMISSION_ERROR\"}}", false, false, ""},
		{"chat raw JSON uppercase code", "openai stream error: {\"code\":\"INVALID_API_KEY\"}", false, false, ""},
		{"chat colon before the detail", "openai stream error: Rate limited: retry later (type=invalid_request_error)", false, false, ""},
		{"chat parenthesis inside the message", "openai stream error: Bad value (x) for tool_choice (type=invalid_request_error)", false, false, ""},
		{"chat code-only 404", "openai stream error: {\"code\":404}", false, false, ""},
		{"chat bare-string code", "openai stream error: invalid_api_key", false, false, ""},
		{"chat bare-string status", "openai stream error: 400", false, false, ""},
		{"status leading the message", "openai stream error: 400: Bad request", false, false, ""},
		{"a permanent verdict is not overridden by its wording", "openai stream error: connection error while validating (type=invalid_request_error)", false, false, ""},
		{"responses quota", "openai stream error: insufficient_quota: pay up", false, false, "Quota exceeded"},
		{"responses failed quota", "openai response failed: insufficient_quota: pay up", false, false, "Quota exceeded"},
		{"responses uppercase permanent code", "openai response failed: INVALID_PROMPT: rejected", false, false, ""},
		{"a permanent type carried as the code", "openai response failed: authentication_error: bad key", false, false, ""},
		{"an empty nested error beside a permanent code", "openai stream error: {\"error\":null,\"code\":\"invalid_api_key\"}", false, false, ""},
		{"an empty nested object beside a 4xx", "openai stream error: {\"error\":{},\"code\":404}", false, false, ""},

		// transport, as before
		{"claw idle watchdog", "openai stream stalled: no data for 5m0s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", true, false, ""},
		{"claw responses idle watchdog", "openai responses stream stalled: no data for 5m0s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", true, false, ""},
		{"truncated stream", "openai stream truncated: closed without finish_reason or [DONE]", true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyStreamEventError(tc.msg)
			if err == nil {
				t.Fatal("no error")
			}
			if got := isRetryable(err); got != tc.retryable {
				t.Errorf("isRetryable = %v, want %v (err %T %v)", got, tc.retryable, err, err)
			}
			var overflow *ContextOverflowError
			if got := errors.As(err, &overflow); got != tc.overflow {
				t.Errorf("context overflow = %v, want %v (err %T %v)", got, tc.overflow, err, err)
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q, want it to say %q", err.Error(), tc.contains)
			}
			var apiErr *APIError
			if !tc.overflow && tc.contains == "" && !errors.As(err, &apiErr) {
				t.Errorf("error %T %v, want a typed *APIError", err, err)
			}
		})
	}
}

// A Responses API failure code keeps its verdict in both forms claw
// reports it in: the refusals the API documents (openai-python
// ResponseError.code) and the terminal codes OpenAI's Codex client maps to
// a dedicated error are permanent; every other code — the transient ones,
// and any code not known here, as Codex does — is retried. flex_unavailable,
// which Codex holds terminal, is a capacity condition OpenAI documents as
// retryable with backoff.
func TestClassifyStreamEventError_ResponsesErrorCodes(t *testing.T) {
	permanent := []string{
		"invalid_prompt", "data_residency_mismatch", "bio_policy", "misalignment_policy_violation",
		"invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url", "image_too_large",
		"image_too_small", "image_parse_error", "image_content_policy_violation", "invalid_image_mode",
		"image_file_too_large", "unsupported_image_media_type", "empty_image_file", "image_file_not_found",
		"cyber_policy", "usage_not_included", "credit_balance_exhausted", "organization_spend_limit_exceeded",
		"project_spend_limit_exceeded",
	}
	retried := []string{
		"server_error", "rate_limit_exceeded", "vector_store_timeout", "failed_to_download_image",
		"slow_down", "request_timeout", "server_is_overloaded", "flex_unavailable", "a_code_nobody_documented",
	}
	for _, set := range []struct {
		codes     []string
		retryable bool
	}{{permanent, false}, {retried, true}} {
		for _, code := range set.codes {
			for _, prefix := range []string{"openai response failed: ", "openai stream error: "} {
				msg := prefix + code + ": details"
				if got := isRetryable(classifyStreamEventError(msg)); got != set.retryable {
					t.Errorf("%q: isRetryable = %v, want %v", msg, got, set.retryable)
				}
			}
		}
	}
}

// A status the provider names as its code travels as the error's HTTP status,
// which the retry event reports.
func TestClassifyStreamEventError_CarriesTheStatusItNames(t *testing.T) {
	for msg, want := range map[string]int{
		"openai stream error: upstream unavailable (code=503)": 503,
		"openai response failed: 429: slow down":               429,
		"openai stream error: boom (type=server_error)":        0,
	} {
		var apiErr *APIError
		if err := classifyStreamEventError(msg); !errors.As(err, &apiErr) || apiErr.StatusCode != want {
			t.Errorf("%q: error %#v, want status %d", msg, err, want)
		}
	}
}
