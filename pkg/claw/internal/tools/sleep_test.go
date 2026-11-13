package tools

import (
	"strings"
	"testing"
)

func TestSleep(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]any
		wantErr string
		wantOut string
	}{
		{
			name:    "happy path 10ms",
			input:   map[string]any{"duration_ms": float64(10)},
			wantOut: "Slept for 10 ms",
		},
		{
			name:    "zero duration succeeds",
			input:   map[string]any{"duration_ms": float64(0)},
			wantOut: "Slept for 0 ms",
		},
		{
			name:    "missing duration_ms",
			input:   map[string]any{},
			wantErr: "'duration_ms' is required",
		},
		{
			name:    "negative duration",
			input:   map[string]any{"duration_ms": float64(-1)},
			wantErr: "must be a non-negative integer",
		},
		{
			name:    "exceeds max",
			input:   map[string]any{"duration_ms": float64(300001)},
			wantErr: "exceeds maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ExecuteSleep(tt.input)
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
