package api

import "testing"

// A budget marker is read in the shape LiteLLM prints it ("Budget has been
// exceeded!", "ExceededBudget: …"), so a gateway's echo of the request — a
// prompt quoting the words, a tool named onExceededBudget — is no budget.
// The three echo bodies of the #3048 closure review.
func TestEchoedBudgetWordsAreNoBudget(t *testing.T) {
	fastapiEcho := `{"detail":[{"type":"missing","loc":["body","model"],"msg":"Field required","input":{"messages":[{"role":"user","content":"Review this diff: + var budgetRefusalMarkers = []string{\"budget has been exceeded\"}"}]}}]}`
	for _, tc := range []struct {
		name          string
		status        int
		message, body string
	}{
		{"422 FastAPI echo", 422, fastapiEcho, fastapiEcho},
		{"400 echo of the prompt", 400, `1 validation error ... input_value='Review: budget has been exceeded'`, `{"object":"error","message":"1 validation error ... input_value='Review: budget has been exceeded'","type":"BadRequestError","code":400}`},
		{"400 tool named onExceededBudget", 400, "Invalid schema for function 'onExceededBudget': In context=(), 'additionalProperties' is required to be supplied and to be false.", `{"error":{"message":"Invalid schema for function 'onExceededBudget': ...","type":"invalid_request_error"}}`},
	} {
		if got := ClassifyRefusal(tc.status, tc.message, tc.body); got == RefusalBudget {
			t.Errorf("%s: an echo read as a budget refusal", tc.name)
		}
	}
}
