package api

import (
	"encoding/json"
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
	// RefusalRequest: a 4xx status that refuses the request as such
	// (requestRefusalStatuses).
	RefusalRequest RefusalKind = "request"
)

// budgetRefusalMarkers are spend-ceiling refusals in the provider's own
// words: LiteLLM's key/team/user budgets ("Budget has been exceeded! Current
// cost: 150.18, Max budget: 150.0", "ExceededBudget: User=… over budget",
// "ExceededBudget: Key over 1d budget", "exceeded budget for model=…", its
// ExceededTokenBudget form) and Anthropic's exhausted credit balance.
// LiteLLM's budget_exceeded error TYPE is read from the envelope's type field
// alone (errorEnvelope), never as a substring: a message may quote the
// request's own identifiers ("function 'notify_budget_exceeded'").
var budgetRefusalMarkers = []string{"budget has been exceeded", "exceededbudget", "exceededtokenbudget", "exceeded budget for model", "credit balance is too low"}

// budgetRefusalType is LiteLLM's error.type for every budget refusal.
const budgetRefusalType = "budget_exceeded"

// requestRefusalStatuses are the 4xx that refuse the request as such: the
// method, a gone endpoint, a missing length, an oversized URI, the media
// type, an unprocessable body, oversized headers. Every other 4xx keeps its
// own reading: 400, whose vLLM catch-all can be decided by the sample; 408,
// 409, 421, 424, 425, 429 and a proxy's 499, which a retry can clear; 401
// and 403, which a fresh credential can; 404, the model lookup; 413, a
// payload a compaction can shrink.
var requestRefusalStatuses = map[int]bool{405: true, 410: true, 411: true, 414: true, 415: true, 422: true, 431: true}

// errorEnvelope reads a provider's JSON error body — {"error":{"message",
// "type"}} or the flat {"message","type"} — and reports whether it was one.
func errorEnvelope(body string) (message, typ string, ok bool) {
	var env struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &env) != nil {
		return "", "", false
	}
	if env.Error != nil {
		return env.Error.Message, env.Error.Type, true
	}
	return env.Message, env.Type, true
}

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
// response's status, its extracted message and its raw body. The markers are
// read on the provider's words only: the message, and from the body the
// envelope's message and type when it is a JSON envelope, or the whole body
// when it is not (a gateway's plain-text answer IS its message). The
// refusal's words decide before its status, because gateways serve them
// under any status (LiteLLM: a budget as 400, 422 or 429, a grammar refusal
// as 500); 402 Payment Required is a budget by status alone.
func ClassifyRefusal(statusCode int, message, body string) RefusalKind {
	words, typ := message, ""
	if envMessage, envType, ok := errorEnvelope(body); ok {
		words += "\n" + envMessage
		typ = strings.ToLower(strings.TrimSpace(envType))
	} else {
		words += "\n" + body
	}
	switch {
	case statusCode == 402 || typ == budgetRefusalType || containsAnyFold(words, budgetRefusalMarkers):
		return RefusalBudget
	case containsAnyFold(words, capabilityRefusalMarkers):
		return RefusalCapability
	case containsAnyFold(words, grammarRefusalMarkers):
		return RefusalGrammar
	case requestRefusalStatuses[statusCode]:
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

// refusalPrecedence orders the refusal kinds RefusalOf decides between when
// err's chain holds several: the remedy an operator acts on first — money,
// then the model, then the schema, then the request.
var refusalPrecedence = []RefusalKind{RefusalBudget, RefusalCapability, RefusalGrammar, RefusalRequest}

// RefusalsOf returns every provider error of err's chain that names a
// refusal, in walk order — joined errors (an exhausted fallback chain)
// included, never descending into a provider error itself.
func RefusalsOf(err error) []ProviderRefusal {
	var out []ProviderRefusal
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if r, ok := e.(ProviderRefusal); ok {
			if r.Refusal() != RefusalNone {
				out = append(out, r)
			}
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// RefusalOf reports the refusal err's chain names: among every provider
// error of the chain (RefusalsOf), the kind first in refusalPrecedence, so
// the verdict never depends on the order its errors were joined in;
// RefusalNone when the chain names none. Every layer above the provider —
// the in-node retry, node recovery, the runner's redelivery and the run's
// automatic resume — reads a refusal through here, from the typed errors the
// chain carries, never from text.
func RefusalOf(err error) RefusalKind {
	met := map[RefusalKind]bool{}
	for _, r := range RefusalsOf(err) {
		met[r.Refusal()] = true
	}
	for _, kind := range refusalPrecedence {
		if met[kind] {
			return kind
		}
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
