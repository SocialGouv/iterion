package apikit

import (
	"fmt"
	"strings"
	"time"
)

// ErrorKind discriminates the 12 ApiError variants.
type ErrorKind int

const (
	// ErrMissingCredentials — no credentials found for the provider.
	ErrMissingCredentials ErrorKind = iota
	// ErrContextWindowExceeded — request exceeds model context window.
	ErrContextWindowExceeded
	// ErrExpiredOAuthToken — saved OAuth token is expired.
	ErrExpiredOAuthToken
	// ErrAuth — authentication/authorization error.
	ErrAuth
	// ErrInvalidAPIKeyEnv — failed to read credential environment variable.
	ErrInvalidAPIKeyEnv
	// ErrHTTP — transport-level HTTP error (connection, timeout).
	ErrHTTP
	// ErrIO — filesystem or other I/O error.
	ErrIO
	// ErrJSON — failed to parse provider response.
	ErrJSON
	// ErrAPI — provider returned a non-success status with a body.
	ErrAPI
	// ErrRetriesExhausted — all retry attempts failed.
	ErrRetriesExhausted
	// ErrInvalidSSEFrame — malformed SSE frame.
	ErrInvalidSSEFrame
	// ErrBackoffOverflow — retry backoff calculation overflowed.
	ErrBackoffOverflow
)

var genericFatalWrapperMarkers = []string{
	"something went wrong while processing your request",
	"please try again, or use /new to start a fresh session",
}

var contextWindowErrorMarkers = []string{
	"maximum context length",
	"context window",
	"context length",
	"too many tokens",
	"prompt is too long",
	"input is too long",
	"request is too large",
}

// ApiError is a structured error covering all API failure modes.
// Fields are populated according to Kind — see per-field godoc.
type ApiError struct {
	Kind ErrorKind

	// MissingCredentials fields
	Provider string   // Provider name (MissingCredentials, JSON)
	EnvVars  []string // Expected env vars (MissingCredentials)
	Hint     string   // Optional runtime hint (MissingCredentials)

	// ContextWindowExceeded fields
	Model                 string // Model name (ContextWindowExceeded, JSON)
	EstimatedInputTokens  uint32 // (ContextWindowExceeded)
	RequestedOutputTokens uint32 // (ContextWindowExceeded)
	EstimatedTotalTokens  uint32 // (ContextWindowExceeded)
	ContextWindowTokens   uint32 // (ContextWindowExceeded)

	// Auth
	AuthMessage string // (Auth)

	// HTTP / IO / JSON
	Cause error // Wrapped error (HTTP, IO, JSON, InvalidAPIKeyEnv)

	// JSON-specific
	BodySnippet string // First 200 chars of response body (JSON)

	// API
	StatusCode   int    // HTTP status code (API)
	ErrorType    string // API error type (API)
	Message      string // API error message (API)
	APIRequestID string // Request ID from headers (API)
	Body         string // Raw response body (API)
	RetryableAPI bool   // Server indicated retryable (API)

	// RetriesExhausted
	Attempts  uint32    // Number of attempts (RetriesExhausted)
	LastError *ApiError // Inner error (RetriesExhausted)

	// InvalidSSEFrame
	SSEMessage string // (InvalidSSEFrame)

	// BackoffOverflow
	OverflowAttempt uint32        // (BackoffOverflow)
	BaseDelay       time.Duration // (BackoffOverflow)
}

// Error implements the error interface.
func (e *ApiError) Error() string {
	switch e.Kind {
	case ErrMissingCredentials:
		msg := fmt.Sprintf("missing %s credentials; export %s before calling the %s API",
			e.Provider, strings.Join(e.EnvVars, " or "), e.Provider)
		if e.Hint != "" {
			msg += " — hint: " + e.Hint
		}
		return msg
	case ErrContextWindowExceeded:
		return fmt.Sprintf(
			"context_window_blocked for %s: estimated input %d + requested output %d = %d tokens exceeds the %d-token context window; compact the session or reduce request size before retrying",
			e.Model, e.EstimatedInputTokens, e.RequestedOutputTokens,
			e.EstimatedTotalTokens, e.ContextWindowTokens)
	case ErrExpiredOAuthToken:
		return "saved OAuth token is expired and no refresh token is available"
	case ErrAuth:
		return "auth error: " + e.AuthMessage
	case ErrInvalidAPIKeyEnv:
		return "failed to read credential environment variable: " + e.Cause.Error()
	case ErrHTTP:
		return "http error: " + e.Cause.Error()
	case ErrIO:
		return "io error: " + e.Cause.Error()
	case ErrJSON:
		return fmt.Sprintf("failed to parse %s response for model %s: %v; first 200 chars of body: %s",
			e.Provider, e.Model, e.Cause, e.BodySnippet)
	case ErrAPI:
		var sb strings.Builder
		if e.ErrorType != "" && e.Message != "" {
			fmt.Fprintf(&sb, "api returned %d (%s)", e.StatusCode, e.ErrorType)
			if e.APIRequestID != "" {
				fmt.Fprintf(&sb, " [trace %s]", e.APIRequestID)
			}
			fmt.Fprintf(&sb, ": %s", e.Message)
		} else {
			fmt.Fprintf(&sb, "api returned %d", e.StatusCode)
			if e.APIRequestID != "" {
				fmt.Fprintf(&sb, " [trace %s]", e.APIRequestID)
			}
			fmt.Fprintf(&sb, ": %s", e.Body)
		}
		return sb.String()
	case ErrRetriesExhausted:
		return fmt.Sprintf("api failed after %d attempts: %s", e.Attempts, e.LastError)
	case ErrInvalidSSEFrame:
		return "invalid sse frame: " + e.SSEMessage
	case ErrBackoffOverflow:
		return fmt.Sprintf("retry backoff overflowed on attempt %d with base delay %v",
			e.OverflowAttempt, e.BaseDelay)
	default:
		return "unknown api error"
	}
}

// Unwrap returns the underlying cause for error chain support.
func (e *ApiError) Unwrap() error {
	switch e.Kind {
	case ErrHTTP, ErrIO, ErrJSON, ErrInvalidAPIKeyEnv:
		return e.Cause
	case ErrRetriesExhausted:
		return e.LastError
	default:
		return nil
	}
}

// IsRetryable reports whether the error is worth retrying.
func (e *ApiError) IsRetryable() bool {
	switch e.Kind {
	case ErrHTTP:
		// Deliberate superset of Rust's is_connect() || is_timeout() || is_request()
		// discrimination on reqwest::Error. In Go, net/http errors that reach
		// ErrHTTP are almost always transport-level (connect refused, timeout,
		// TLS handshake failure), so treating all of them as retryable is a safe
		// simplification that avoids coupling to specific net/http error types.
		return true
	case ErrAPI:
		return e.RetryableAPI
	case ErrRetriesExhausted:
		if e.LastError != nil {
			return e.LastError.IsRetryable()
		}
		return false
	default:
		return false
	}
}

// RequestID returns the provider request ID if available.
func (e *ApiError) RequestID() string {
	switch e.Kind {
	case ErrAPI:
		return e.APIRequestID
	case ErrRetriesExhausted:
		if e.LastError != nil {
			return e.LastError.RequestID()
		}
		return ""
	default:
		return ""
	}
}

// SafeFailureClass returns a safe classification string for telemetry.
// Possible values: "context_window", "provider_retry_exhausted",
// "provider_auth", "provider_rate_limit", "provider_internal",
// "provider_error", "provider_transport", "runtime_io".
func (e *ApiError) SafeFailureClass() string {
	switch e.Kind {
	case ErrRetriesExhausted:
		if e.IsContextWindowFailure() {
			return "context_window"
		}
		if e.IsGenericFatalWrapper() {
			return "provider_retry_exhausted"
		}
		if e.LastError != nil {
			return e.LastError.SafeFailureClass()
		}
		return "provider_error"
	case ErrMissingCredentials, ErrExpiredOAuthToken, ErrAuth:
		return "provider_auth"
	case ErrContextWindowExceeded:
		return "context_window"
	case ErrAPI:
		if e.StatusCode == 401 || e.StatusCode == 403 {
			return "provider_auth"
		}
		if e.IsContextWindowFailure() {
			return "context_window"
		}
		if e.StatusCode == 429 {
			return "provider_rate_limit"
		}
		if e.IsGenericFatalWrapper() {
			return "provider_internal"
		}
		return "provider_error"
	case ErrHTTP, ErrInvalidSSEFrame, ErrBackoffOverflow:
		return "provider_transport"
	case ErrInvalidAPIKeyEnv, ErrIO, ErrJSON:
		return "runtime_io"
	default:
		return "provider_error"
	}
}

// IsContextWindowFailure checks if this error indicates a context window overflow.
func (e *ApiError) IsContextWindowFailure() bool {
	switch e.Kind {
	case ErrContextWindowExceeded:
		return true
	case ErrAPI:
		if e.StatusCode != 400 && e.StatusCode != 413 && e.StatusCode != 422 {
			return false
		}
		return looksLikeContextWindowError(e.Message) || looksLikeContextWindowError(e.Body)
	case ErrRetriesExhausted:
		if e.LastError != nil {
			return e.LastError.IsContextWindowFailure()
		}
		return false
	default:
		return false
	}
}

// IsGenericFatalWrapper checks for generic server-side error wrappers.
func (e *ApiError) IsGenericFatalWrapper() bool {
	switch e.Kind {
	case ErrAPI:
		return looksLikeGenericFatalWrapper(e.Message) || looksLikeGenericFatalWrapper(e.Body)
	case ErrRetriesExhausted:
		if e.LastError != nil {
			return e.LastError.IsGenericFatalWrapper()
		}
		return false
	default:
		return false
	}
}

// NewMissingCredentials creates a MissingCredentials error.
func NewMissingCredentials(provider string, envVars []string) *ApiError {
	return &ApiError{Kind: ErrMissingCredentials, Provider: provider, EnvVars: envVars}
}

// NewMissingCredentialsWithHint creates a MissingCredentials error with a hint.
func NewMissingCredentialsWithHint(provider string, envVars []string, hint string) *ApiError {
	return &ApiError{Kind: ErrMissingCredentials, Provider: provider, EnvVars: envVars, Hint: hint}
}

// NewJSONDeserializeError creates a JSON deserialization error with context.
func NewJSONDeserializeError(provider, model, body string, cause error) *ApiError {
	return &ApiError{
		Kind:        ErrJSON,
		Provider:    provider,
		Model:       model,
		BodySnippet: TruncateBodySnippet(body, 200),
		Cause:       cause,
	}
}

// TruncateBodySnippet truncates a body to at most maxChars runes, appending "…" if truncated.
func TruncateBodySnippet(body string, maxChars int) string {
	runes := []rune(body)
	if len(runes) <= maxChars {
		return body
	}
	return string(runes[:maxChars]) + "…"
}

// IsRetryableStatus returns true for HTTP status codes that warrant a retry.
func IsRetryableStatus(statusCode int) bool {
	switch statusCode {
	case 408, 409, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func looksLikeContextWindowError(text string) bool {
	if text == "" {
		return false
	}
	lowered := strings.ToLower(text)
	for _, marker := range contextWindowErrorMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

func looksLikeGenericFatalWrapper(text string) bool {
	if text == "" {
		return false
	}
	lowered := strings.ToLower(text)
	for _, marker := range genericFatalWrapperMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
