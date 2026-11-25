package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

// #3048: the three production refusals of 2026-11-25 (iterion.cloud, LiteLLM
// in front of vLLM), as the claw client types them. Each one met the same
// answer on every attempt, and each one bought claw retries, node recoveries,
// redeliveries or automatic resumes.
var refusalCases = []struct {
	name     string
	err      *api.APIError
	code     runtime.ErrorCode
	inRemedy string
}{
	{
		name: "budget as 422 (run 01a211c3: 4 redeliveries, then the DLQ)",
		err: &api.APIError{Provider: "openai", StatusCode: 422,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0",
			Body:    `{"error":{"message":"Budget has been exceeded! Current cost: 150.18, Max budget: 150.0","type":"budget_exceeded","param":null,"code":"422"}}`},
		code:     runtime.ErrCodeProviderBudgetExceeded,
		inRemedy: "raise that budget",
	},
	{
		name: "budget as 500",
		err: &api.APIError{Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		code:     runtime.ErrCodeProviderBudgetExceeded,
		inRemedy: "raise that budget",
	},
	{
		name: "budget as 429",
		err: &api.APIError{Provider: "openai", StatusCode: 429, Retryable: true,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		code:     runtime.ErrCodeProviderBudgetExceeded,
		inRemedy: "raise that budget",
	},
	{
		name: "not multimodal (run 01a211cd-79f7: resumed automatically)",
		err: &api.APIError{Provider: "openai", StatusCode: 400,
			Message: "litellm.BadRequestError: OpenAIException - glm-5.3 is not a multimodal model"},
		code:     runtime.ErrCodeCapabilityUnsupported,
		inRemedy: "a model that accepts it",
	},
	{
		name: "grammar as 500 (run 01a211cd-78df: 3 retries x 7 recoveries x 3 resumes)",
		err: &api.APIError{Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty:  (And (SearchRegex /^x$/) (Regex /(?s:.{1,253})/))"},
		code:     runtime.ErrCodeSchemaUnusable,
		inRemedy: "decoding grammar",
	},
	{
		name:     "422 refusing the request",
		err:      &api.APIError{Provider: "openai", StatusCode: 422, Message: "Unprocessable Entity: field required"},
		code:     runtime.ErrCodeProviderRequestInvalid,
		inRemedy: "refused the request itself",
	},
}

func TestDeterministicRefusalStopsEveryLayer(t *testing.T) {
	for _, tc := range refusalCases {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("model: node %q: backend %q failed: %w", "review", "claw", fmt.Errorf("claw backend: runner: %w", tc.err))
			// The claw client and the executor's retry read IsRetryable.
			if tc.err.IsRetryable() {
				t.Errorf("claw layer: IsRetryable = true")
			}
			// Node recovery.
			action, code := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
			if code != tc.code {
				t.Fatalf("code = %s, want %s", code, tc.code)
			}
			if action.Kind != runtime.RecoveryFailTerminal {
				t.Errorf("node recovery = %v (%s), want FailTerminal", action.Kind, action.Reason)
			}
			if !strings.Contains(action.Reason, tc.inRemedy) || !strings.Contains(action.Reason, tc.err.Message) {
				t.Errorf("the remedy does not name the cause: %s", action.Reason)
			}
			// Redelivery (pkg/runner) and the CLI's --auto-resume read the
			// one table: deterministic, never auto-resumed — the run stays
			// failed_resumable for the operator.
			if !retrypolicy.IsDeterministic(code) || retrypolicy.AutoResumable(code) {
				t.Errorf("disposition of %s = %v, want deterministic", code, retrypolicy.Classify(code))
			}
			// The run's failure message carries the same remedy.
			wrapped := &runtime.RuntimeError{Code: code, Message: "node failed", Cause: err}
			if hint := runtime.RemedyOf(wrapped).Hint; !strings.Contains(hint, tc.inRemedy) {
				t.Errorf("RemedyOf hint = %q", hint)
			}
		})
	}
}

// A stream error relayed from the sandboxed runner is read like its HTTP
// form, whatever retryable flag the stream decoder gave it.
func TestDeterministicRefusalFromAStreamError(t *testing.T) {
	stream := &model.APIError{StatusCode: 500, IsRetryable: true,
		Message: "stream error: Grammar error: Unimplemented keys: [\"propertyNames\"]"}
	if got := recovery.Classify(fmt.Errorf("claw backend: runner: %w", stream)); got != runtime.ErrCodeSchemaUnusable {
		t.Errorf("stream grammar refusal = %s, want SCHEMA_UNUSABLE", got)
	}
}

// A refusal condemns one route: another key, model or gateway may serve what
// it refused. The chain's verdict is the same in both orders of its routes.
func TestRefusedRouteInAChainIsRouteLocal(t *testing.T) {
	budget := refusalCases[0].err
	grammar := refusalCases[4].err
	down := &api.APIError{Provider: "openai", StatusCode: 503, Message: "Service Unavailable", Retryable: true}
	for _, tc := range []struct {
		name   string
		routes []error
		want   runtime.ErrorCode
	}{
		{"budget then down", []error{budget, down}, runtime.ErrCodeNetworkTransient},
		{"down then budget", []error{down, budget}, runtime.ErrCodeNetworkTransient},
		{"budget then grammar", []error{budget, grammar}, runtime.ErrCodeProviderBudgetExceeded},
		{"grammar then budget", []error{grammar, budget}, runtime.ErrCodeProviderBudgetExceeded},
		{"grammar twice", []error{grammar, grammar}, runtime.ErrCodeSchemaUnusable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recovery.Classify(r3Chain(tc.routes)); got != tc.want {
				t.Errorf("chain = %s, want %s", got, tc.want)
			}
		})
	}
}

// Only the typed error proves a refusal: the same words as bare text keep
// the reading they always had.
func TestFlattenedRefusalTextIsNotARefusal(t *testing.T) {
	flat := errors.New("claw backend: runner: openai: API error 422: Budget has been exceeded! Current cost: 150.18, Max budget: 150.0")
	if got := recovery.Classify(flat); got == runtime.ErrCodeProviderBudgetExceeded {
		t.Errorf("a flattened text was read as a typed refusal: %s", got)
	}
}
