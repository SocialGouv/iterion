package tools

import (
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	tests := []struct {
		name      string
		input     map[string]any
		configMap map[string]any
		wantErr   string
		wantOut   string
	}{
		{
			name:      "key found in config map",
			input:     map[string]any{"key": "model"},
			configMap: map[string]any{"model": "claude-4", "temperature": 0.7},
			wantOut:   `"found": true`,
		},
		{
			name:      "key not found",
			input:     map[string]any{"key": "missing_key"},
			configMap: map[string]any{"model": "claude-4"},
			wantOut:   `"found": false`,
		},
		{
			name:      "missing key parameter",
			input:     map[string]any{},
			configMap: map[string]any{"model": "claude-4"},
			wantErr:   "'key' is required",
		},
		{
			name:      "nil config map",
			input:     map[string]any{"key": "model"},
			configMap: nil,
			wantErr:   "no configuration available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ExecuteConfig(tt.input, tt.configMap)
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
