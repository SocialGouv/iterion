package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
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
