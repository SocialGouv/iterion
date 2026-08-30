package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestLaunchRunRequestFallbackAcceptsObjectAndArray(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantModels []string
	}{
		{
			name:       "legacy object",
			raw:        `{"fallback":{"backend":"codex","model":"gpt-5.5"}}`,
			wantModels: []string{"gpt-5.5"},
		},
		{
			name: "ordered array",
			raw: `{"fallback":[` +
				`{"backend":"codex","model":"gpt-5.5"},` +
				`{"backend":"claw","model":"openai/gpt-5.5"}]}`,
			wantModels: []string{"gpt-5.5", "openai/gpt-5.5"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req launchRunRequest
			if err := json.Unmarshal([]byte(tc.raw), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(req.Fallback) != len(tc.wantModels) {
				t.Fatalf("fallback = %+v, want %d stages", req.Fallback, len(tc.wantModels))
			}
			for i, want := range tc.wantModels {
				if req.Fallback[i].Model != want {
					t.Fatalf("fallback[%d].model = %q, want %q", i, req.Fallback[i].Model, want)
				}
			}
			blob, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Contains(blob, []byte(`"fallback":[`)) {
				t.Fatalf("canonical launch fallback is not an array: %s", blob)
			}
		})
	}
}

func TestLaunchRunRequestFallbackRejectsScalar(t *testing.T) {
	var req launchRunRequest
	if err := json.Unmarshal([]byte(`{"fallback":"codex:gpt-5.5"}`), &req); err == nil {
		t.Fatal("scalar fallback must be rejected explicitly")
	}
}
