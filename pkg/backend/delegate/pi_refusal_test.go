package delegate

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func piFailure(t *testing.T, status int, message string) pisdk.Message {
	t.Helper()
	diag, err := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": message}})
	if err != nil {
		t.Fatal(err)
	}
	var m pisdk.Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","stopReason":"error","errorMessage":`+string(mustJSON(t, message))+`,"diagnostics":[`+string(diag)+`]}`), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// #3048 backend parity: pi reads its provider's refusal through the same
// classifier as claw (api.ClassifyRefusal) before its status switch — a
// grammar refusal served as a 500 is no transient fault, a not-multimodal
// 400 is typed, and a budget refusal is the spend ceiling every backend parks
// on, whatever status the gateway gave it.
func TestPiTypesProviderRefusalsLikeClaw(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
		want    api.RefusalKind
	}{
		{"grammar 500", 500, "litellm.APIConnectionError: OpenAIException - Grammar error: Unable to determine if regex is empty", api.RefusalGrammar},
		{"multimodal 400", 400, "litellm.BadRequestError: OpenAIException - glm-5.3 is not a multimodal model", api.RefusalCapability},
		{"request 422", 422, "Unprocessable Entity", api.RefusalRequest},
	} {
		err := piClassifyFailure(piFailure(t, tc.status, tc.message))
		var typed *api.APIError
		if !errors.As(err, &typed) || api.RefusalOf(err) != tc.want || typed.IsRetryable() {
			t.Errorf("%s: %T %v — want a non-retryable typed %s refusal", tc.name, err, err, tc.want)
		}
		var transient *ErrTransient
		if errors.As(err, &transient) {
			t.Errorf("%s: typed transient", tc.name)
		}
	}
	for _, status := range []int{400, 422, 429, 500} {
		err := piClassifyFailure(piFailure(t, status, "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"))
		if !IsUsageWindow(err) || !IsAccountRefusal(err) {
			t.Errorf("budget %d: %T %v — want the spend-ceiling account refusal", status, err, err)
		}
	}
}
