package api

import "fmt"

// APIError is the canonical typed error returned by provider clients
// (anthropic, openai, ...) when an upstream API call returns a non-2xx
// HTTP response. Providers must populate StatusCode and (where possible)
// extract Message from the upstream error envelope so external callers
// (e.g. iterion) can drive retry/classification via errors.As without
// parsing free-form strings.
type APIError struct {
	// Provider names the upstream that produced the error
	// (e.g. "openai", "anthropic"). Empty when not applicable.
	Provider string

	// StatusCode is the HTTP status code returned by the API.
	StatusCode int

	// Message is a short human-readable description, ideally extracted
	// from the API's own error envelope when one is present.
	Message string

	// Body is the raw response body, truncated to a reasonable size
	// for diagnostics. Avoid leaking large prompts back into logs.
	Body string

	// Retryable is true iff the StatusCode falls into the standard
	// transient-status set (408, 409, 429, 5xx).
	Retryable bool
}

// Error returns a stable string representation:
// "<provider>: API error <status>: <message>".
func (e *APIError) Error() string {
	if e == nil {
		return "<nil APIError>"
	}
	if e.Provider != "" {
		return fmt.Sprintf("%s: API error %d: %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Message)
}

// IsRetryable reports whether the error is in the standard transient set.
func (e *APIError) IsRetryable() bool {
	if e == nil {
		return false
	}
	return e.Retryable
}

// IsRetryableStatus returns true for HTTP status codes that warrant a
// retry: 408 (Request Timeout), 409 (Conflict — usually transient lock
// contention), 429 (Too Many Requests), and 500-599 (server errors).
func IsRetryableStatus(statusCode int) bool {
	switch statusCode {
	case 408, 409, 429:
		return true
	}
	return statusCode >= 500 && statusCode <= 599
}
