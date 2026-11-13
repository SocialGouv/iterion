package recovery

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"strings"
	"testing"
)

func TestR2ProviderBadRequestDisposition(t *testing.T) {
	for _, tc := range []struct {
		name, message, code string
		status              int
	}{
		{"effort", "DeepSeek V4.1 reasoning_effort must be low, high, xhigh, max, or an integer within [1, 100] in chat_template_kwargs", "PROVIDER_REQUEST_INVALID", 400},
		{"generic", "bad request", "EXECUTION_FAILED", 400},
		{"context", "context_length_exceeded", "CONTEXT_LENGTH_EXCEEDED", 400},
		{"context prose", "maximum context length exceeded", "CONTEXT_LENGTH_EXCEEDED", 400},
		{"model gate", "model requires a newer version", "MODEL_UNAVAILABLE", 400},
		{"timeout", "request timed out", "NETWORK_TRANSIENT", 408},
		{"overload", "overload", "NETWORK_TRANSIENT", 503},
		{"throttle", "too many requests", "RATE_LIMITED", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &api.APIError{StatusCode: tc.status, Message: tc.message}
			if got := Classify(err); string(got) != tc.code {
				t.Fatalf("code=%s want=%s", got, tc.code)
			}
			if tc.code == "PROVIDER_REQUEST_INVALID" {
				flat := errors.New(err.Error())
				if got := Classify(flat); got != runtime.ErrCodeExecutionFailed {
					t.Errorf("flattened code=%s", got)
				}
				action, code := Dispatch(DefaultRecipes())(context.Background(), err, nil)
				if action.Kind != runtime.RecoveryFailTerminal || !strings.Contains(action.Reason, tc.message) {
					t.Errorf("invalid request retry/remedy: %+v", action)
				}
				if !retrypolicy.IsDeterministic(code) {
					t.Errorf("cloud retries invalid request: %s", code)
				}
			}
		})
	}
}
