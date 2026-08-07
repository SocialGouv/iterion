package server

import (
	"encoding/json"
	"testing"
)

// TestResumeRequest_CarriesFallbackRoute pins the wire contract of the
// resume endpoint's `fallback` field.
//
// The route is NOT persisted on the run (like the launch-time backend and
// permission overrides), so a resume that says nothing runs without one —
// and a resume is frequently the very moment the primary credential is
// still walled, which is the whole scenario ADR-087 exists for. The field
// therefore has to be re-statable on every resume surface: `iterion resume
// --fallback` on the CLI, this JSON key over HTTP. Without it the
// ResumeSpec.Fallback the service, the detached subprocess and the cloud
// publisher all read would have no producer at all outside the CLI.
//
// Scope: the decode contract (key name and the backend/model/provider
// triple), which is what an API client depends on. The pass-through into
// runview.ResumeSpec is a direct field assignment in handleResumeRun.
func TestResumeRequest_CarriesFallbackRoute(t *testing.T) {
	var req resumeRunRequest
	body := `{"file_path":"/tmp/demo.bot","fallback":{"backend":"claw","model":"openai/gpt-5.5","provider":"openai"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Fallback == nil {
		t.Fatal("resume dropped the fallback route: an operator re-stating --fallback over HTTP would be silently ignored")
	}
	if got := req.Fallback.Backend; got != "claw" {
		t.Errorf("backend = %q, want %q", got, "claw")
	}
	if got := req.Fallback.Model; got != "openai/gpt-5.5" {
		t.Errorf("model = %q, want %q", got, "openai/gpt-5.5")
	}
	if got := req.Fallback.Provider; got != "openai" {
		t.Errorf("provider = %q, want %q", got, "openai")
	}

	// A resume that expresses no route decodes to nil, which every
	// consumer (toRunFallback, toQueueFallback, fallbackFlagValue) reads
	// as "the caller expressed none" — each node then keeps whatever its
	// own `fallbacks:` block declares.
	var none resumeRunRequest
	if err := json.Unmarshal([]byte(`{"file_path":"/tmp/demo.bot"}`), &none); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if none.Fallback != nil {
		t.Errorf("absent fallback decoded to %+v, want nil", none.Fallback)
	}
}
