package api

import (
	"errors"
	"fmt"
	"strings"
)

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
	// transient-status set (408, 409, 429, 5xx). IsRetryable, not this
	// field, is the verdict: it also refuses a deterministic refusal.
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

// IsRetryable reports whether the error is in the standard transient set and
// names no deterministic refusal (Refusal): the stored flag says what the
// status said, and a refusal served as a 5xx or a 429 meets the same answer
// on every retry. Read here, every retry loop shares the verdict, whatever
// built the error or carried it across a process.
func (e *APIError) IsRetryable() bool {
	if e == nil {
		return false
	}
	return e.Retryable && e.Refusal() == RefusalNone
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

// RefusalKind names a provider refusal no retry can clear: the same request
// meets it again, whatever status it is served with. RefusalNone is the zero
// value — no refusal named, the status decides as usual.
type RefusalKind string

const (
	RefusalNone RefusalKind = ""
	// RefusalBudget: the spend ceiling of the gateway key, team or account
	// is reached. Only an operator raising it (or buying credits) changes
	// the answer.
	RefusalBudget RefusalKind = "budget"
	// RefusalCapability: the model cannot take an input kind the request
	// carries (an image sent to a text-only model). Only another model or
	// another input changes the answer.
	RefusalCapability RefusalKind = "capability"
	// RefusalGrammar: the gateway's constrained-decoding engine cannot
	// compile the request's schemas into a grammar. Only another schema, or
	// a request asking for no grammar, changes the answer.
	RefusalGrammar RefusalKind = "grammar"
	// RefusalRequest: a 4xx status that refuses the request as such (405,
	// 415, 422, …). Every 4xx except 400, whose vLLM catch-all can be
	// decided by the sample; 408, 409 and 429, which a retry can clear;
	// 401 and 403, which a fresh credential can; 404, the model lookup; and
	// 413, a payload a compaction can shrink — each keeps its own reading.
	RefusalRequest RefusalKind = "request"
)

// budgetRefusalMarkers are spend-ceiling refusals in the provider's own
// words: LiteLLM's key/team/user budgets ("Budget has been exceeded! Current
// cost: 150.18, Max budget: 150.0", its ExceededTokenBudget form and its
// budget_exceeded error type) and Anthropic's exhausted credit balance.
var budgetRefusalMarkers = []string{"budget has been exceeded", "exceededtokenbudget", "budget_exceeded", "credit balance is too low"}

// capabilityRefusalMarkers name a model refusing an input kind it cannot
// take: vLLM through LiteLLM ("glm-5.3 is not a multimodal model") and
// OpenAI's image_url refusal on a text-only model.
var capabilityRefusalMarkers = []string{"is not a multimodal model", "image_url is only supported by certain models"}

// grammarRefusalMarkers name a gateway's constrained-decoding engine refusing
// to compile a request's JSON schemas into a grammar — what a forced
// tool_choice or a structured output makes vLLM do: xgrammar ("features not
// supported by xgrammar") and the "Grammar error:" family ("Unimplemented
// keys", llguidance's "Unable to determine if regex is empty"). LiteLLM serves
// some of them as a 500.
var grammarRefusalMarkers = []string{"xgrammar", "grammar error"}

// ClassifyRefusal is the one reading of a provider refusal, from the
// response's status, its extracted message and its raw body. The refusal's
// own words decide before its status, because gateways serve them under any
// status (LiteLLM: a budget as 400, 422 or 429, a grammar refusal as 500);
// 402 Payment Required is a budget by status alone.
func ClassifyRefusal(statusCode int, message, body string) RefusalKind {
	switch {
	case statusCode == 402 || containsAnyFold(message, budgetRefusalMarkers) || containsAnyFold(body, budgetRefusalMarkers):
		return RefusalBudget
	case containsAnyFold(message, capabilityRefusalMarkers) || containsAnyFold(body, capabilityRefusalMarkers):
		return RefusalCapability
	case containsAnyFold(message, grammarRefusalMarkers) || containsAnyFold(body, grammarRefusalMarkers):
		return RefusalGrammar
	}
	switch statusCode {
	case 400, 401, 403, 404, 408, 409, 413, 429:
		return RefusalNone
	}
	if statusCode > 400 && statusCode < 500 {
		return RefusalRequest
	}
	return RefusalNone
}

// Refusal reports the deterministic refusal this error names, RefusalNone
// when it names none.
func (e *APIError) Refusal() RefusalKind {
	if e == nil {
		return RefusalNone
	}
	return ClassifyRefusal(e.StatusCode, e.Message, e.Body)
}

// IsGrammarRefusal reports whether the gateway refused to compile the
// request's schemas into a decoding grammar. The refusal is deterministic:
// the same request meets it again, so it is never retryable as such; what
// can succeed is a request that asks for no grammar.
func (e *APIError) IsGrammarRefusal() bool {
	return e.Refusal() == RefusalGrammar
}

// ProviderRefusal is implemented by every typed provider error that can name
// a deterministic refusal: *APIError for an HTTP response, and the stream
// error type of a caller that decodes a provider error from a stream event.
type ProviderRefusal interface {
	error
	Refusal() RefusalKind
}

// RefusalOf reports the refusal the first provider error of err's chain
// names; RefusalNone when the chain holds none, or that error names none.
// Every layer above the provider — the in-node retry, node recovery, the
// runner's redelivery and the run's automatic resume — reads a refusal
// through here, from the typed error the chain carries, never from text.
func RefusalOf(err error) RefusalKind {
	var refusal ProviderRefusal
	if errors.As(err, &refusal) {
		return refusal.Refusal()
	}
	return RefusalNone
}

// NewHTTPError builds the APIError of a provider's non-2xx response.
// Retryable follows the status, except for a refusal (ClassifyRefusal): a
// status alone does not tell it from a transient fault.
func NewHTTPError(provider string, statusCode int, message, body string) *APIError {
	e := &APIError{Provider: provider, StatusCode: statusCode, Message: message, Body: body}
	e.Retryable = IsRetryableStatus(statusCode) && e.Refusal() == RefusalNone
	return e
}

func containsAnyFold(s string, markers []string) bool {
	s = strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// RequestValidationError reports an invalid or unsupported local request before
// any HTTP call. Unlike APIError it does not represent a provider response.
type RequestValidationError struct {
	Field  string
	Reason string
}

func (e *RequestValidationError) Error() string        { return e.Field + ": " + e.Reason }
func (e *RequestValidationError) InvalidField() string { return e.Field }
func (e *RequestValidationError) IsRetryable() bool    { return false }
