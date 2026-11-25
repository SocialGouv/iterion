package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
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
//
// A budget refusal follows the spend-ceiling doctrine (an account refusal:
// USAGE_LIMIT_BLOCKED, the run parks and an hourly probe relaunches it — a
// gateway budget often reopens by itself); every other refusal is
// deterministic, parked for the operator.
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
		code:     runtime.ErrCodeUsageLimitBlocked,
		inRemedy: "raise that budget",
	},
	{
		name: "budget as 500",
		err: &api.APIError{Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		code:     runtime.ErrCodeUsageLimitBlocked,
		inRemedy: "raise that budget",
	},
	{
		name: "budget as 429",
		err: &api.APIError{Provider: "openai", StatusCode: 429, Retryable: true,
			Message: "ExceededBudget: Key over 1d budget. Spend=5.01, Budget=5.0"},
		code:     runtime.ErrCodeUsageLimitBlocked,
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

func TestProviderRefusalStopsEveryLayer(t *testing.T) {
	for _, tc := range refusalCases {
		t.Run(tc.name, func(t *testing.T) {
			// The claw client and the executor's retry read IsRetryable.
			if tc.err.IsRetryable() {
				t.Errorf("claw layer: IsRetryable = true")
			}
			bare := fmt.Errorf("claw backend: runner: %w", tc.err)
			for _, cause := range []error{bare, spendTyped(bare)} {
				err := fmt.Errorf("model: node %q: backend %q failed: %w", "review", "claw", cause)
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
				if code == runtime.ErrCodeUsageLimitBlocked {
					// The spend ceiling parks as an account refusal: the
					// runner arms the usage park, the hourly probe relaunches.
					if !delegate.IsAccountRefusal(err) || retrypolicy.IsDeterministic(code) {
						t.Errorf("a budget refusal is not parked as an account refusal (account=%v, disposition=%v)", delegate.IsAccountRefusal(err), retrypolicy.Classify(code))
					}
				} else if !retrypolicy.IsDeterministic(code) || retrypolicy.AutoResumable(code) {
					// Redelivery (pkg/runner) and the CLI's --auto-resume
					// read the one table: deterministic, never auto-resumed.
					t.Errorf("disposition of %s = %v, want deterministic", code, retrypolicy.Classify(code))
				}
				// The run's failure message carries the same remedy.
				wrapped := &runtime.RuntimeError{Code: code, Message: "node failed", Cause: err}
				if hint := runtime.RemedyOf(wrapped).Hint; !strings.Contains(hint, tc.inRemedy) {
					t.Errorf("RemedyOf hint = %q", hint)
				}
			}
		})
	}
}

// spendTyped is what the claw boundary hands up for a budget refusal
// (delegate.SpendCeilingRefusal); any other error stays as it is.
func spendTyped(err error) error {
	if spend, ok := delegate.SpendCeilingRefusal(delegate.BackendClaw, err); ok {
		return spend
	}
	return err
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
// it refused. The chain's verdict is the same in both orders of its routes;
// a budget route is read as the spend ceiling it is (chainWaitsOnUsageWindow).
func TestRefusedRouteInAChainIsRouteLocal(t *testing.T) {
	budget := spendTyped(refusalCases[0].err)
	capability := refusalCases[3].err
	grammar := refusalCases[4].err
	down := &api.APIError{Provider: "openai", StatusCode: 503, Message: "Service Unavailable", Retryable: true}
	auth := &delegate.ErrAuthFailed{}
	for _, tc := range []struct {
		name   string
		routes []error
		want   runtime.ErrorCode
	}{
		{"grammar then down", []error{grammar, down}, runtime.ErrCodeNetworkTransient},
		{"down then grammar", []error{down, grammar}, runtime.ErrCodeNetworkTransient},
		{"capability then grammar", []error{capability, grammar}, runtime.ErrCodeCapabilityUnsupported},
		{"grammar then capability", []error{grammar, capability}, runtime.ErrCodeCapabilityUnsupported},
		{"grammar twice", []error{grammar, grammar}, runtime.ErrCodeSchemaUnusable},
		{"budget then down", []error{budget, down}, runtime.ErrCodeUsageLimitBlocked},
		{"down then budget", []error{down, budget}, runtime.ErrCodeUsageLimitBlocked},
		{"budget then grammar", []error{budget, grammar}, runtime.ErrCodeUsageLimitBlocked},
		{"budget then dead credential", []error{budget, auth}, runtime.ErrCodeAuthFailed},
		{"dead credential then budget", []error{auth, budget}, runtime.ErrCodeAuthFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recovery.Classify(r3Chain(tc.routes)); got != tc.want {
				t.Errorf("chain = %s, want %s", got, tc.want)
			}
		})
	}
}

// #3048 M3: a chain refused two ways keeps the code of refusalPrecedence, and
// its remedy names THAT refusal — never the other route's, in either order.
func TestRefusedChainRemedyNamesTheRefusalOfItsCode(t *testing.T) {
	capability := refusalCases[3].err
	grammar := refusalCases[4].err
	for _, routes := range [][]error{{capability, grammar}, {grammar, capability}} {
		err := r3Chain(routes)
		action, code := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, nil)
		if code != runtime.ErrCodeCapabilityUnsupported {
			t.Fatalf("code = %s, want CAPABILITY_UNSUPPORTED", code)
		}
		hint := runtime.RemedyOf(&runtime.RuntimeError{Code: code, Message: "node failed", Cause: err}).Hint
		for _, text := range []string{action.Reason, hint} {
			if !strings.Contains(text, capability.Message) || strings.Contains(text, "Grammar error") || strings.Contains(text, "decoding grammar") {
				t.Errorf("routes %v: the remedy does not name the capability refusal alone: %s", routes, text)
			}
		}
	}
}

// Only the typed error proves a refusal: the same words as bare text keep
// the reading they always had.
func TestFlattenedRefusalTextIsNotARefusal(t *testing.T) {
	flat := errors.New("claw backend: runner: openai: API error 422: Unprocessable Entity: glm-5.3 is not a multimodal model")
	if got := recovery.Classify(flat); got == runtime.ErrCodeCapabilityUnsupported {
		t.Errorf("a flattened text was read as a typed refusal: %s", got)
	}
}
