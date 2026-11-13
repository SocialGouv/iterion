package delegate

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func TestR3PiPreservesTyped400(t *testing.T) {
	var msg pisdk.Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","stopReason":"error","errorMessage":"reasoning_effort must be low, high, xhigh, max","diagnostics":[{"error":{"code":400,"message":"reasoning_effort must be low, high, xhigh, max"}}]}`), &msg); err != nil {
		t.Fatal(err)
	}
	err := piClassifyFailure(msg)
	var typed *api.APIError
	if !errors.As(err, &typed) || typed.StatusCode != 400 {
		t.Fatalf("400 lost: %T %v (status=%d)", err, err, msg.HTTPStatus())
	}
}
