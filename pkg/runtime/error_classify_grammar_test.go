package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// A gateway's grammar refusal is deterministic, whatever status it is served
// under: LiteLLM's 500 must not reach the network-transient backoff and its
// automatic resumes (#3679). It condemns the route, not the chain, and the
// remedy quotes the refusal.
func TestGrammarRefusalIsAPermanentRouteLocalVerdict(t *testing.T) {
	refusal500 := &api.APIError{Provider: "openai", StatusCode: 500, Message: "litellm.InternalServerError", Body: `{"error":{"message":"Grammar error: Unable to determine if regex is empty"}}`}
	refusal400 := &api.APIError{Provider: "openai", StatusCode: 400, Message: "The provided JSON schema contains features not supported by xgrammar."}
	outage := &api.APIError{Provider: "openai", StatusCode: 503, Message: "overloaded"}
	chain := func(errs ...error) error { return &model.ErrChainExhausted{Chain: "a,b", Errs: errs} }
	for name, tc := range map[string]struct {
		err  error
		want ErrorCode
	}{
		"500 grammar error":         {refusal500, ErrCodeProviderRequestInvalid},
		"400 xgrammar":              {refusal400, ErrCodeProviderRequestInvalid},
		"wrapped":                   {fmt.Errorf("node agent: %w", refusal500), ErrCodeProviderRequestInvalid},
		"plain 503 stays transient": {outage, ErrCodeNetworkTransient},
		"every route refused":       {chain(refusal500, refusal400), ErrCodeProviderRequestInvalid},
		"a rescue route's outage":   {chain(refusal500, outage), ErrCodeNetworkTransient},
	} {
		if got := ClassifyError(tc.err); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if remedy := ProviderRequestRemedy(refusal400); !strings.Contains(remedy, "xgrammar") {
		t.Errorf("remedy = %q, want the gateway's refusal quoted", remedy)
	}
}
