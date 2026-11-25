package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

func TestR3RunnerEncodesProviderIdentity(t *testing.T) {
	const message = "DeepSeek V4.1 reasoning_effort must be low, high, xhigh, max, or an integer within [1, 100] in chat_template_kwargs"
	for _, cause := range []error{&api.APIError{Provider: "openai", StatusCode: 400, Message: message}, &model.APIError{StatusCode: 400, Message: message}} {
		encoded, err := json.Marshal(clawRunnerResult(delegate.Result{}, fmt.Errorf("structured generation: %w", cause)))
		if err != nil {
			t.Fatal(err)
		}
		var crossed delegate.IOResult
		if err := json.Unmarshal(encoded, &crossed); err != nil {
			t.Fatal(err)
		}
		if crossed.ProviderError == nil || runtime.ClassifyError(crossed.ProviderError) != runtime.ErrCodeProviderRequestInvalid {
			t.Fatalf("provider lost crossing: %s", encoded)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		_, stream := cause.(*model.APIError)
		if got := fields["provider_error_stream"] == true; got != stream {
			t.Errorf("provider source lost crossing: stream=%v wire=%s", stream, encoded)
		}
	}
	untyped := clawRunnerResult(delegate.Result{}, errors.New("model prose: status code 400 reasoning_effort must be high"))
	if untyped.ProviderError != nil {
		t.Fatalf("prose minted a provider error: %+v", untyped)
	}
}

// #3048: a deterministic refusal crosses the runner typed — an HTTP one as
// any provider error does, a stream one whatever its status — so the
// launcher's node recovery and the runner's redelivery read the refusal from
// the type rather than from the diagnostic string.
func TestRunnerEncodesADeterministicRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		want  runtime.ErrorCode
	}{
		"budget 422": {&api.APIError{Provider: "openai", StatusCode: 422,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"}, runtime.ErrCodeProviderBudgetExceeded},
		"stream grammar 500": {&model.APIError{StatusCode: 500, IsRetryable: true,
			Message: "stream error: Grammar error: Unable to determine if regex is empty"}, runtime.ErrCodeSchemaUnusable},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(clawRunnerResult(delegate.Result{}, fmt.Errorf("claw backend: text+tools generation: %w", tc.cause)))
			if err != nil {
				t.Fatal(err)
			}
			var crossed delegate.IOResult
			if err := json.Unmarshal(encoded, &crossed); err != nil {
				t.Fatal(err)
			}
			if crossed.ProviderError == nil {
				t.Fatalf("the refusal crossed as text only: %s", encoded)
			}
			if got := runtime.ClassifyError(crossed.ProviderError); got != tc.want {
				t.Errorf("crossed refusal = %s, want %s", got, tc.want)
			}
			if crossed.ProviderError.IsRetryable() {
				t.Errorf("crossed refusal is retryable: %s", encoded)
			}
		})
	}
}
