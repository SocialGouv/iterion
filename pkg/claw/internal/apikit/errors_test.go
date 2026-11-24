package apikit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIsRetryableAcrossAllKinds(t *testing.T) {
	tests := []struct {
		name     string
		err      *ApiError
		expected bool
	}{
		{"MissingCredentials", &ApiError{Kind: ErrMissingCredentials}, false},
		{"ContextWindowExceeded", &ApiError{Kind: ErrContextWindowExceeded}, false},
		{"ExpiredOAuthToken", &ApiError{Kind: ErrExpiredOAuthToken}, false},
		{"Auth", &ApiError{Kind: ErrAuth}, false},
		{"InvalidAPIKeyEnv", &ApiError{Kind: ErrInvalidAPIKeyEnv, Cause: fmt.Errorf("not set")}, false},
		{"HTTP", &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("conn reset")}, true},
		{"IO", &ApiError{Kind: ErrIO, Cause: fmt.Errorf("disk full")}, false},
		{"JSON", &ApiError{Kind: ErrJSON, Cause: fmt.Errorf("parse err")}, false},
		{"API retryable", &ApiError{Kind: ErrAPI, RetryableAPI: true, StatusCode: 502}, true},
		{"API non-retryable", &ApiError{Kind: ErrAPI, RetryableAPI: false, StatusCode: 400}, false},
		{"RetriesExhausted retryable inner", &ApiError{Kind: ErrRetriesExhausted, LastError: &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("timeout")}}, true},
		{"RetriesExhausted non-retryable inner", &ApiError{Kind: ErrRetriesExhausted, LastError: &ApiError{Kind: ErrAuth}}, false},
		{"InvalidSSEFrame", &ApiError{Kind: ErrInvalidSSEFrame, SSEMessage: "bad frame"}, false},
		{"BackoffOverflow", &ApiError{Kind: ErrBackoffOverflow}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.IsRetryable(); got != tt.expected {
				t.Errorf("IsRetryable() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSafeFailureClassAcrossKinds(t *testing.T) {
	tests := []struct {
		name     string
		err      *ApiError
		expected string
	}{
		{"MissingCredentials", &ApiError{Kind: ErrMissingCredentials}, "provider_auth"},
		{"ExpiredOAuthToken", &ApiError{Kind: ErrExpiredOAuthToken}, "provider_auth"},
		{"Auth", &ApiError{Kind: ErrAuth}, "provider_auth"},
		{"ContextWindowExceeded", &ApiError{Kind: ErrContextWindowExceeded}, "context_window"},
		{"API 401", &ApiError{Kind: ErrAPI, StatusCode: 401}, "provider_auth"},
		{"API 403", &ApiError{Kind: ErrAPI, StatusCode: 403}, "provider_auth"},
		{"API 429", &ApiError{Kind: ErrAPI, StatusCode: 429}, "provider_rate_limit"},
		{"API context window", &ApiError{Kind: ErrAPI, StatusCode: 400, Message: "maximum context length exceeded"}, "context_window"},
		{"API generic", &ApiError{Kind: ErrAPI, StatusCode: 500, Message: "Something went wrong while processing your request"}, "provider_internal"},
		{"API other", &ApiError{Kind: ErrAPI, StatusCode: 500}, "provider_error"},
		{"HTTP", &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("err")}, "provider_transport"},
		{"InvalidSSEFrame", &ApiError{Kind: ErrInvalidSSEFrame}, "provider_transport"},
		{"BackoffOverflow", &ApiError{Kind: ErrBackoffOverflow}, "provider_transport"},
		{"InvalidAPIKeyEnv", &ApiError{Kind: ErrInvalidAPIKeyEnv, Cause: fmt.Errorf("err")}, "runtime_io"},
		{"IO", &ApiError{Kind: ErrIO, Cause: fmt.Errorf("err")}, "runtime_io"},
		{"JSON", &ApiError{Kind: ErrJSON, Cause: fmt.Errorf("err")}, "runtime_io"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.SafeFailureClass(); got != tt.expected {
				t.Errorf("SafeFailureClass() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRetriesExhaustedFailureClasses(t *testing.T) {
	// Context window through retries
	ctxErr := &ApiError{
		Kind:      ErrRetriesExhausted,
		Attempts:  3,
		LastError: &ApiError{Kind: ErrAPI, StatusCode: 400, Message: "maximum context length exceeded", RetryableAPI: false},
	}
	if ctxErr.SafeFailureClass() != "context_window" {
		t.Errorf("expected context_window, got %s", ctxErr.SafeFailureClass())
	}

	// Generic fatal through retries
	fatalErr := &ApiError{
		Kind:     ErrRetriesExhausted,
		Attempts: 3,
		LastError: &ApiError{
			Kind: ErrAPI, StatusCode: 500, RetryableAPI: true,
			Message: "Something went wrong while processing your request. Please try again, or use /new to start a fresh session.",
		},
	}
	if fatalErr.SafeFailureClass() != "provider_retry_exhausted" {
		t.Errorf("expected provider_retry_exhausted, got %s", fatalErr.SafeFailureClass())
	}
}

func TestIsContextWindowFailureDetectsAllMarkers(t *testing.T) {
	markers := []string{
		"maximum context length", "context window", "context length",
		"too many tokens", "prompt is too long", "input is too long", "request is too large",
	}
	for _, marker := range markers {
		err := &ApiError{Kind: ErrAPI, StatusCode: 400, Message: "Error: " + marker + " exceeded"}
		if !err.IsContextWindowFailure() {
			t.Errorf("IsContextWindowFailure should detect marker %q", marker)
		}
	}
}

func TestIsContextWindowOnlyForRelevantStatusCodes(t *testing.T) {
	for _, status := range []int{400, 413, 422} {
		err := &ApiError{Kind: ErrAPI, StatusCode: status, Message: "maximum context length exceeded"}
		if !err.IsContextWindowFailure() {
			t.Errorf("status %d should trigger context window detection", status)
		}
	}

	err := &ApiError{Kind: ErrAPI, StatusCode: 500, Message: "maximum context length exceeded"}
	if err.IsContextWindowFailure() {
		t.Error("status 500 should NOT trigger context window detection")
	}
}

func TestUnwrapChaining(t *testing.T) {
	cause := fmt.Errorf("connection refused")
	httpErr := &ApiError{Kind: ErrHTTP, Cause: cause}
	if !errors.Is(httpErr, cause) {
		t.Error("Unwrap should allow errors.Is to find the cause")
	}

	innerErr := &ApiError{Kind: ErrAPI, StatusCode: 502, RetryableAPI: true}
	retriesErr := &ApiError{Kind: ErrRetriesExhausted, Attempts: 3, LastError: innerErr}

	var target *ApiError
	if !errors.As(retriesErr, &target) {
		t.Error("errors.As should find ApiError through Unwrap chain")
	}
}

func TestRequestIDThroughRetries(t *testing.T) {
	inner := &ApiError{Kind: ErrAPI, StatusCode: 502, APIRequestID: "req_nested_456", RetryableAPI: true}
	outer := &ApiError{Kind: ErrRetriesExhausted, Attempts: 3, LastError: inner}

	if outer.RequestID() != "req_nested_456" {
		t.Errorf("expected req_nested_456, got %s", outer.RequestID())
	}
}

func TestErrorMessages(t *testing.T) {
	tests := []struct {
		name     string
		err      *ApiError
		contains string
	}{
		{
			"MissingCredentials",
			NewMissingCredentials("Anthropic", []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"}),
			"missing Anthropic credentials",
		},
		{
			"MissingCredentialsWithHint",
			NewMissingCredentialsWithHint("Anthropic", []string{"ANTHROPIC_API_KEY"}, "try openai/"),
			"hint: try openai/",
		},
		{
			"ContextWindowExceeded",
			&ApiError{Kind: ErrContextWindowExceeded, Model: "claude-opus-4-6", EstimatedInputTokens: 190000, RequestedOutputTokens: 32000, EstimatedTotalTokens: 222000, ContextWindowTokens: 200000},
			"context_window_blocked",
		},
		{
			"ExpiredOAuth",
			&ApiError{Kind: ErrExpiredOAuthToken},
			"OAuth token is expired",
		},
		{
			"BackoffOverflow",
			&ApiError{Kind: ErrBackoffOverflow, OverflowAttempt: 99, BaseDelay: time.Second},
			"backoff overflowed",
		},
		{
			"RetriesExhausted",
			&ApiError{Kind: ErrRetriesExhausted, Attempts: 3, LastError: &ApiError{Kind: ErrHTTP, Cause: fmt.Errorf("timeout")}},
			"api failed after 3 attempts",
		},
		{
			"API with type and message",
			&ApiError{Kind: ErrAPI, StatusCode: 500, ErrorType: "api_error", Message: "internal", APIRequestID: "req_123"},
			"[trace req_123]",
		},
		{
			"InvalidSSEFrame",
			&ApiError{Kind: ErrInvalidSSEFrame, SSEMessage: "bad frame"},
			"invalid sse frame",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.Error()
			if !strings.Contains(msg, tt.contains) {
				t.Errorf("error message %q should contain %q", msg, tt.contains)
			}
		})
	}
}

func TestTruncateBodySnippet(t *testing.T) {
	t.Run("short body unchanged", func(t *testing.T) {
		if got := TruncateBodySnippet("hello", 200); got != "hello" {
			t.Errorf("expected 'hello', got %q", got)
		}
	})

	t.Run("empty body unchanged", func(t *testing.T) {
		if got := TruncateBodySnippet("", 200); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("long body truncated", func(t *testing.T) {
		body := strings.Repeat("a", 250)
		snippet := TruncateBodySnippet(body, 200)
		runes := []rune(snippet)
		if len(runes) != 201 { // 200 + ellipsis
			t.Errorf("expected 201 runes, got %d", len(runes))
		}
		if !strings.HasSuffix(snippet, "…") {
			t.Error("should end with ellipsis")
		}
	})

	t.Run("multibyte safe", func(t *testing.T) {
		body := "한글한글한글한글한글한글"
		snippet := TruncateBodySnippet(body, 4)
		if snippet != "한글한글…" {
			t.Errorf("expected '한글한글…', got %q", snippet)
		}
	})
}

func TestJSONDeserializeError(t *testing.T) {
	rawBody := strings.Repeat("x", 190) + "_TAIL_PAST_200_CHARS_MARKER_"
	cause := fmt.Errorf("parse error")
	err := NewJSONDeserializeError("Anthropic", "claude-opus-4-6", rawBody, cause)

	msg := err.Error()
	if !strings.HasPrefix(msg, "failed to parse Anthropic response for model claude-opus-4-6:") {
		t.Errorf("unexpected prefix: %s", msg)
	}
	if !strings.Contains(msg, "first 200 chars of body:") {
		t.Error("should contain body snippet label")
	}
	if strings.Contains(msg, "_TAIL_PAST_200_CHARS_MARKER_") {
		t.Error("should not contain text past 200-char cap")
	}
	if err.SafeFailureClass() != "runtime_io" {
		t.Errorf("expected runtime_io, got %s", err.SafeFailureClass())
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{408, 409, 429, 500, 502, 503, 504}
	for _, code := range retryable {
		if !IsRetryableStatus(code) {
			t.Errorf("status %d should be retryable", code)
		}
	}
	nonRetryable := []int{200, 201, 400, 401, 403, 404, 422}
	for _, code := range nonRetryable {
		if IsRetryableStatus(code) {
			t.Errorf("status %d should not be retryable", code)
		}
	}
}
