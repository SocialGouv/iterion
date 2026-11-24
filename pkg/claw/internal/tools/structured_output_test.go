package tools

import (
	"strings"
	"testing"
)

func TestStructuredOutput(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]any
		wantErr string
		wantOut string
	}{
		{
			name:    "happy path with payload wrapper",
			input:   map[string]any{"payload": map[string]any{"key": "value", "count": float64(42)}},
			wantOut: "Structured output provided successfully",
		},
		{
			name:    "happy path bare keys (back-compat with non-wrapped callers)",
			input:   map[string]any{"key": "value"},
			wantOut: "Structured output provided successfully",
		},
		{
			name:    "empty payload",
			input:   map[string]any{},
			wantErr: "payload must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ExecuteStructuredOutput(tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out, tt.wantOut) {
				t.Fatalf("output %q does not contain %q", out, tt.wantOut)
			}
		})
	}
}

func TestStructuredOutputToolDescribesTerminalUse(t *testing.T) {
	description := StructuredOutputTool().Description
	for _, required := range []string{"ENDS the session", "ONLY after the mission is fully complete", "NEVER use it to describe planned or remaining work"} {
		if !strings.Contains(description, required) {
			t.Errorf("description %q does not contain %q", description, required)
		}
	}
}
