package runtime

import (
	"errors"
	"regexp"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// Keep the parent's context verdicts in one list for both typed API paths.
// In particular a bare "prompt is too long" retains EXECUTION_FAILED and
// its fresh-sample retry; it never becomes an operator-parameter refusal.
var providerContextMarkers = []string{"context_length_exceeded", "context length"}

func matchesProviderContext(message string) bool {
	for _, marker := range providerContextMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// The closed list reads ONLY a typed provider message. No status or error
// class (notably vLLM's catch-all BadRequestError) proves a request defect.
// Unsupported parameter/value must name one of the request controls below;
// messages/tool results/images and content-policy refusals stay outside it.
var providerParameterRefusals = []*regexp.Regexp{
	regexp.MustCompile(`\breasoning_effort must be\b`),
	regexp.MustCompile(`\bunsupported (?:parameter|value):? ["'\x60]?(?:reasoning_effort|temperature|top_p|top_k|max_tokens|max_completion_tokens|presence_penalty|frequency_penalty|seed|stop|logprobs|top_logprobs|n|stream|response_format)["'\x60]?(?:[ :,.]|$)`),
	regexp.MustCompile(`\bmax_(?:tokens|completion_tokens) is too large\b`),
}

func invalidProviderParameter(message string) bool {
	message = strings.ToLower(message)
	if matchesProviderContext(message) || strings.Contains(message, "messages[") || strings.Contains(message, "messages.") {
		return false
	}
	for _, pattern := range providerParameterRefusals {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// routeLocalCodes are the verdicts that condemn ONE route rather than the
// node: an operator-parameter refusal (the closed list) and an answer that
// stayed off its schema after its corrections. Another route of the chain
// can still serve either way.
var routeLocalCodes = map[ErrorCode]bool{
	ErrCodeProviderRequestInvalid: true,
	ErrCodeSchemaValidation:       true,
}

// classifyRefusedChain decides an exhausted fallback chain that holds at
// least one route-local verdict (routeLocalCodes). Only a chain whose EVERY
// route ended that way keeps one of those codes — SCHEMA_VALIDATION when any
// route's answer stayed off its schema, else PROVIDER_REQUEST_INVALID, in
// whatever order the routes ran. Otherwise the routes that failed for
// another reason decide, read like any other chain: a rescue route's
// transient outage stays transient. It declines (decided=false) a chain with
// no route-local verdict — the reading of ClassifyError stays the one it
// always had — and a chain a RuntimeError encloses, whose code is the
// engine's own verdict on the whole chain.
func classifyRefusedChain(err error) (code ErrorCode, decided bool) {
	chain := exhaustedChainToRead(err)
	if chain == nil {
		return "", false
	}
	var others []error
	schema := false
	for _, cause := range chain.Errs {
		c := ClassifyError(cause)
		if !routeLocalCodes[c] {
			others = append(others, cause)
			continue
		}
		if c == ErrCodeSchemaValidation {
			schema = true
		}
	}
	switch len(others) {
	case 0:
		if schema {
			return ErrCodeSchemaValidation, true
		}
		return ErrCodeProviderRequestInvalid, true
	case len(chain.Errs):
		return "", false
	}
	return ClassifyError(&model.ErrChainExhausted{Chain: chain.Chain, Errs: others}), true
}

// exhaustedChainToRead is the exhausted fallback chain a chain reading may
// decide, or nil: no chain in err, an empty one, or one a RuntimeError
// encloses — whose code is the engine's own verdict on the whole chain, so no
// reading of its routes overrides it.
func exhaustedChainToRead(err error) *model.ErrChainExhausted {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) || len(chain.Errs) == 0 {
		return nil
	}
	var enclosing *RuntimeError
	if errors.As(err, &enclosing) {
		var inner *model.ErrChainExhausted
		if errors.As(enclosing.Cause, &inner) && inner == chain {
			return nil
		}
	}
	return chain
}

// chainWaitsOnUsageWindow reports an exhausted fallback chain that holds at
// least one route refused on a provider usage window waiting cures, whatever
// its other routes say. The node parks on the window (USAGE_LIMIT_BLOCKED)
// and the run resumes when it reopens; a credential another route found dead
// is a fact the diagnostic names (ChainRouteVerdicts), not the reason for the
// stop. Read BEFORE the credential check, on every route: errors.As over
// Unwrap() []error finds the first *delegate.ErrAuthFailed of the set
// wherever it sits, so a dead route anywhere in the walk used to pause the
// run for a human with a five-hour wait as its whole cause (#3032). A chain
// whose routes are all dead credentials holds no window and keeps
// AUTH_FAILED.
//
// A route refused by the ACCOUNT (a spend ceiling, a fair-usage restriction:
// delegate.IsAccountRefusal) names no reset, and no wait is known to cure it.
// Such routes park the run as a window does — the chain's own verdict, in
// every order of its routes — except beside a dead credential: the human the
// pause calls can revive that route, while hourly blind parks would only
// spend the attempts against two walls that both need one.
func chainWaitsOnUsageWindow(err error) bool {
	chain := exhaustedChainToRead(err)
	if chain == nil {
		return false
	}
	accountRefused, deadCredential := false, false
	for _, cause := range chain.Errs {
		if cause == nil {
			continue
		}
		switch ClassifyError(cause) {
		case ErrCodeUsageLimitBlocked:
			if !delegate.IsAccountRefusal(cause) {
				return true
			}
			accountRefused = true
		case ErrCodeAuthFailed:
			deadCredential = true
		}
	}
	return accountRefused && !deadCredential
}

// UsageWindowRoutes returns the routes of the exhausted fallback chain err
// holds — wherever it sits, a RuntimeError enclosing it included — that a
// provider usage window refused, account refusals included, in walk order;
// nil when err holds no chain or none of its routes met a window. The runner
// reads the instant its usage park waits for from these routes' own refusals:
// the chain's message names its LAST route only, and errors.As finds the
// FIRST refusal of the walk, so neither is a fact about the earliest route to
// reopen.
func UsageWindowRoutes(err error) []error {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) {
		return nil
	}
	var out []error
	for _, route := range chain.Errs {
		if route != nil && ClassifyError(route) == ErrCodeUsageLimitBlocked {
			out = append(out, route)
		}
	}
	return out
}

// ChainRouteVerdicts is the per-route evidence of an exhausted fallback
// chain — each route's own classification and error, in walk order — or nil
// when err holds no such chain. The node's single code says what the run
// does next; this says what every route met, so a dead credential behind a
// usage window stays visible on the node_recovery event.
func ChainRouteVerdicts(err error) []map[string]any {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) || len(chain.Errs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(chain.Errs))
	for i, cause := range chain.Errs {
		verdict := map[string]any{"route": i}
		if cause != nil {
			verdict["code"] = string(ClassifyError(cause))
			verdict["error"] = cause.Error()
		}
		out = append(out, verdict)
	}
	return out
}

func providerParameterCause(err error) string {
	var typed *api.APIError
	if errors.As(err, &typed) && typed.StatusCode == 400 && invalidProviderParameter(typed.Message) {
		return typed.Message
	}
	var stream *model.APIError
	if errors.As(err, &stream) && stream.StatusCode == 400 && invalidProviderParameter(stream.Message) {
		return stream.Message
	}
	return "request parameter rejected"
}
