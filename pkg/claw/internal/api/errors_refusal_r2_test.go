package api

import (
	"errors"
	"fmt"
	"testing"
)

// The provider's own words are read from what the provider SAID — its
// extracted message and its error envelope's type — never from request
// content it echoes: a function named notify_budget_exceeded is no budget.
func TestClassifyRefusalReadsTheProviderWordsNotTheRequest(t *testing.T) {
	const echoed = "Invalid schema for function 'notify_budget_exceeded': In context=('properties', 'level'), 'enum' must have at least one value."
	body := `{"error":{"message":"` + echoed + `","type":"invalid_request_error","param":"tools[0].function.parameters","code":"invalid_function_parameters"}}`
	if got := ClassifyRefusal(400, echoed, body); got != RefusalNone {
		t.Errorf("an echoed function name read as a %q refusal", got)
	}
	// The envelope's TYPE is the provider's verdict, whatever its message says.
	typed := `{"error":{"message":"Spend over limit for this key","type":"budget_exceeded","param":null,"code":"400"}}`
	if got := ClassifyRefusal(400, "Spend over limit for this key", typed); got != RefusalBudget {
		t.Errorf("error.type budget_exceeded = %q, want budget", got)
	}
}

// RefusalRequest is the 4xx that refuse the request as such; a 4xx a retry
// can clear (421 misdirected, 424/425, a proxy's 499) is no refusal.
func TestRefusalRequestIsAnAllowlist(t *testing.T) {
	for _, status := range []int{405, 410, 411, 414, 415, 422, 431} {
		if got := ClassifyRefusal(status, "refused", ""); got != RefusalRequest {
			t.Errorf("%d = %q, want request", status, got)
		}
	}
	for _, status := range []int{421, 424, 425, 499, 418, 451} {
		if got := ClassifyRefusal(status, "refused", ""); got != RefusalNone {
			t.Errorf("%d = %q, want none", status, got)
		}
	}
}

// LiteLLM's other budget refusals, in its own words, under 400 and under the
// 429 its budget_exceeded_status_code knob can serve them as.
func TestLiteLLMBudgetWordings(t *testing.T) {
	for _, msg := range []string{
		"ExceededBudget: User=u-17 over budget. Spend=10.52, Budget=10.0",
		"ExceededBudget: Key over 1d budget. Spend=5.01, Budget=5.0",
		"Authentication Error - exceeded budget for model=glm-5.3",
	} {
		for _, status := range []int{400, 429} {
			if got := ClassifyRefusal(status, msg, ""); got != RefusalBudget {
				t.Errorf("%d %q = %q, want budget", status, msg, got)
			}
		}
	}
}

// RefusalOf reads every provider error of a joined chain, so its verdict does
// not depend on the order the errors were joined in.
func TestRefusalOfIsOrderIndependent(t *testing.T) {
	capability := &APIError{StatusCode: 400, Message: "glm-5.3 is not a multimodal model"}
	grammar := &APIError{StatusCode: 500, Message: "Grammar error: Unimplemented keys"}
	plain := &APIError{StatusCode: 503, Message: "upstream connect error", Retryable: true}
	for _, tc := range []struct {
		a, b error
		want RefusalKind
	}{
		{capability, grammar, RefusalCapability},
		{plain, grammar, RefusalGrammar},
	} {
		for _, joined := range []error{
			fmt.Errorf("chain: %w", errors.Join(tc.a, tc.b)),
			fmt.Errorf("chain: %w", errors.Join(tc.b, tc.a)),
		} {
			if got := RefusalOf(joined); got != tc.want {
				t.Errorf("RefusalOf(%v) = %q, want %q", joined, got, tc.want)
			}
		}
	}
}
