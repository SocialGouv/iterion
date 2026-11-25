package runner

import (
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// refusedNodeFailure is a claw node's failure as the engine hands it to the runner:
// the code runtime.ClassifyError reads, carried on the RuntimeError.
func refusedNodeFailure(cause error) error {
	cause = fmt.Errorf("model: node %q: backend %q failed: %w", "review", "claw", fmt.Errorf("claw backend: runner: %w", cause))
	return fmt.Errorf("runner: %w", &runtime.RuntimeError{
		Code:    runtime.ClassifyError(cause),
		NodeID:  "review",
		Message: "node failed",
		Cause:   cause,
	})
}

// #3048: a node the gateway refused deterministically is ACKed — no
// redelivery, no DLQ park, no synthesised resume. Measured on 2026-11-25: a
// 400 "not a multimodal model" resumed automatically; a 500 grammar refusal
// resumed three times until a manual cancel.
func TestDeterministicRefusalIsNotRedelivered(t *testing.T) {
	for name, provider := range map[string]*api.APIError{
		"not multimodal 400": {Provider: "openai", StatusCode: 400,
			Message: "litellm.BadRequestError: OpenAIException - glm-5.3 is not a multimodal model"},
		"grammar 500": {Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			execErr := refusedNodeFailure(provider)
			out := classifyExecResult(execErr, "run-1")
			if out.action != actionAck || out.finalStatus != "deterministic_failure" {
				t.Errorf("outcome = %s/%v (code %s), want an ACKed deterministic_failure", out.finalStatus, out.action, runtimeCodeOf(execErr))
			}
		})
	}
}

// #3048, the operator's doctrine: a gateway budget refusal is a spend
// ceiling. The runner reads it as usage-window evidence — the run parks, the
// hourly probe relaunches it — instead of redelivering it (measured: a 422
// "Budget has been exceeded" redelivered four times, then the DLQ) or
// condemning it as deterministic.
func TestBudgetRefusalParksOnTheUsageWindow(t *testing.T) {
	for name, provider := range map[string]*api.APIError{
		"422": {Provider: "openai", StatusCode: 422,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		"429": {Provider: "openai", StatusCode: 429, Retryable: true,
			Message: "ExceededBudget: User=u-17 over budget. Spend=10.52, Budget=10.0"},
	} {
		t.Run(name, func(t *testing.T) {
			spend, ok := delegate.SpendCeilingRefusal(delegate.BackendClaw, provider)
			if !ok {
				t.Fatal("not typed as a spend ceiling")
			}
			for _, execErr := range []error{refusedNodeFailure(provider), refusedNodeFailure(spend)} {
				if code := runtimeCodeOf(execErr); code != runtime.ErrCodeUsageLimitBlocked {
					t.Errorf("code = %s, want USAGE_LIMIT_BLOCKED", code)
				}
				if _, parks := usageWindowEvidence(execErr, time.Now()); !parks {
					t.Error("no usage-window evidence: the run would not park on the ceiling")
				}
				if out := classifyExecResult(execErr, "run-1"); out.finalStatus == "deterministic_failure" {
					t.Errorf("a budget refusal was condemned as deterministic: %+v", out)
				}
			}
		})
	}
}
