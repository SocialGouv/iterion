package ingress

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// A preview reads a payload as the lane reads a delivery: the outputs
// extracted, the credential field and every unlisted header gone (the
// canary), and the loop guard's drop said.
func TestAPreviewReadsAsTheLaneReads(t *testing.T) {
	in := &spec.Ingress{
		Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "token", Secret: "vendor"},
		Headers:    []string{"X-Chat-Listed"},
	}
	trig := spec.Trigger{ID: "chat.message.posted", Event: "message",
		Outputs: []spec.TriggerOutput{{Name: "prompt", From: "payload.text"}},
		Ignore:  "payload.room == 'bots'",
	}
	h := http.Header{}
	h.Set("X-Chat-Listed", "seen")
	h.Set("X-Chat-Unlisted", "CANARY-HEADER")
	raw, filtered, err := Preview(in, trig, map[string]any{"token": "CANARY-TOKEN", "text": "ship it", "room": "ops"}, h)
	if err != nil || filtered != "" {
		t.Fatalf("preview = %q, %v", filtered, err)
	}
	got := string(raw)
	for _, canary := range []string{"CANARY-TOKEN", "CANARY-HEADER"} {
		if strings.Contains(got, canary) {
			t.Errorf("the preview carries %s: %s", canary, got)
		}
	}
	if !strings.Contains(got, `"prompt":"ship it"`) || !strings.Contains(got, `"X-Chat-Listed":"seen"`) {
		t.Errorf("the preview = %s, want the output and the listed header", got)
	}
	if _, filtered, err := Preview(in, trig, map[string]any{"text": "x", "room": "bots"}, nil); err != nil || filtered == "" {
		t.Fatalf("a payload the loop guard drops = %q, %v; want it said", filtered, err)
	}
}
