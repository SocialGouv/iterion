package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	claudesdk "github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestModelAccessDenialAcrossCLIBackends(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, body := range []string{
		"API Error: 403 The requested model 'deepseek-v4.1-flash' is not available for this API key",
		"API Error: 401 key not allowed to access model",
		"API Error: 403 Team member not allowed to access model",
		"API Error: 403 Project p does not have access to model gpt-5",
	} {
		for _, backend := range []string{"pi", "claude_code"} {
			var err error
			if backend == "pi" {
				var m pisdk.Message
				status := "403"
				if strings.Contains(body, "401") {
					status = "401"
				}
				if decodeErr := json.Unmarshal([]byte(`{"diagnostics":[{"error":{"code":`+status+`}}]}`), &m); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				m.StopReason = pisdk.StopError
				m.ErrorMessage = body
				err = piClassifyFailure(m)
			} else {
				b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
				err = b.renderedFailure(context.Background(), &claudesdk.ResultMessage{Result: &body}, Task{Model: "deepseek-v4.1-flash"}, "test", credSpawn{})
			}
			var unavailable *ErrModelUnavailable
			if !errors.As(err, &unavailable) {
				t.Errorf("%s: %q -> %v, want ErrModelUnavailable", backend, body, err)
			}
		}
	}
}
