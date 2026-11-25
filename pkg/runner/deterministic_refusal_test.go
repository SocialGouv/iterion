package runner

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// #3048: a node the gateway refused deterministically fails with the code the
// engine classifies (runtime.ClassifyError, carried on the RuntimeError),
// and the runner ACKs it — no redelivery, no DLQ park, no synthesised
// resume. Measured on 2026-11-25: a 422 budget redelivered four times then
// parked; a 400 "not a multimodal model" resumed automatically; a 500
// grammar refusal resumed three times until a manual cancel.
func TestDeterministicRefusalIsNotRedelivered(t *testing.T) {
	for name, provider := range map[string]*api.APIError{
		"budget 422": {Provider: "openai", StatusCode: 422,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		"not multimodal 400": {Provider: "openai", StatusCode: 400,
			Message: "litellm.BadRequestError: OpenAIException - glm-5.3 is not a multimodal model"},
		"grammar 500": {Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "litellm.APIConnectionError: APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			cause := fmt.Errorf("model: node %q: backend %q failed: %w", "review", "claw", fmt.Errorf("claw backend: runner: %w", provider))
			execErr := fmt.Errorf("runner: %w", &runtime.RuntimeError{
				Code:    runtime.ClassifyError(cause),
				NodeID:  "review",
				Message: "node failed",
				Cause:   cause,
			})
			out := classifyExecResult(execErr, "run-1")
			if out.action != actionAck || out.finalStatus != "deterministic_failure" {
				t.Errorf("outcome = %s/%v (code %s), want an ACKed deterministic_failure", out.finalStatus, out.action, runtimeCodeOf(execErr))
			}
		})
	}
}
