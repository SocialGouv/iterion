package runtime

import (
	"errors"
	"regexp"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
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

// classifyRefusedChain decides an exhausted fallback chain that holds at
// least one operator-parameter refusal. Only a chain whose EVERY route was
// refused that way is PROVIDER_REQUEST_INVALID: a rescue route that failed
// otherwise can still serve, so the refused routes are set aside and the
// others decide, read like any other chain. It declines (decided=false) a
// chain no route of which was refused — the reading of ClassifyError stays
// the one it always had — and a chain a RuntimeError encloses, whose code is
// the engine's own verdict on the whole chain.
func classifyRefusedChain(err error) (code ErrorCode, decided bool) {
	var chain *model.ErrChainExhausted
	if !errors.As(err, &chain) || len(chain.Errs) == 0 {
		return "", false
	}
	var enclosing *RuntimeError
	if errors.As(err, &enclosing) {
		var inner *model.ErrChainExhausted
		if errors.As(enclosing.Cause, &inner) && inner == chain {
			return "", false
		}
	}
	var others []error
	for _, cause := range chain.Errs {
		if ClassifyError(cause) != ErrCodeProviderRequestInvalid {
			others = append(others, cause)
		}
	}
	switch len(others) {
	case 0:
		return ErrCodeProviderRequestInvalid, true
	case len(chain.Errs):
		return "", false
	}
	return ClassifyError(&model.ErrChainExhausted{Chain: chain.Chain, Errs: others}), true
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
