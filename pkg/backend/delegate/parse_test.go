package delegate

import (
	"encoding/json"
	"testing"
)

func strptr(s string) *string { return &s }

// TestParseSDKOutput covers the populated-vs-empty structured_output
// distinction that the claude_code Pass-1 fast path relies on: a non-empty
// structured_output is returned directly (no formatting pass), while an empty
// `structured_output: {}` (what a tool session emits when the agent never
// called StructuredOutput) must fall through to the result-text path.
func TestParseSDKOutput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"echoed":{"type":"string"}},"required":["echoed"]}`)
	multiSchema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`)

	tests := []struct {
		name         string
		resultText   *string
		structured   any
		schema       json.RawMessage
		wantKey      string // a key expected in the output map ("" = expect empty map)
		wantVal      string // expected value for wantKey
		wantFallback bool
	}{
		{
			name:       "populated structured_output is returned directly (fast path)",
			resultText: strptr("I returned it in the echoed field."),
			structured: map[string]any{"echoed": "HELLO"},
			schema:     schema,
			wantKey:    "echoed", wantVal: "HELLO", wantFallback: false,
		},
		{
			name:       "empty structured_output falls through to result text",
			resultText: strptr(`{"echoed":"FROM_TEXT"}`),
			structured: map[string]any{},
			schema:     schema,
			wantKey:    "echoed", wantVal: "FROM_TEXT", wantFallback: false,
		},
		{
			name:       "nil structured_output, raw json result text",
			resultText: strptr(`{"echoed":"RAW"}`),
			structured: nil,
			schema:     schema,
			wantKey:    "echoed", wantVal: "RAW", wantFallback: false,
		},
		{
			name:       "nil structured_output, fenced json in markdown",
			resultText: strptr("Here is the result:\n```json\n{\"echoed\":\"FENCED\"}\n```\n"),
			structured: nil,
			schema:     schema,
			wantKey:    "echoed", wantVal: "FENCED", wantFallback: false,
		},
		{
			name:       "plain text with single-string schema wraps under that field (valid)",
			resultText: strptr("just some prose, no json"),
			structured: nil,
			schema:     schema,
			wantKey:    "echoed", wantVal: "just some prose, no json", wantFallback: false,
		},
		{
			name:       "plain text with multi-field schema wraps as fallback",
			resultText: strptr("just some prose, no json"),
			structured: nil,
			schema:     multiSchema,
			wantKey:    "text", wantVal: "just some prose, no json", wantFallback: true,
		},
		{
			name:       "empty structured_output AND empty text yields empty map (drives formatting fallback)",
			resultText: strptr(""),
			structured: map[string]any{},
			schema:     schema,
			wantKey:    "", wantFallback: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _, fallback := parseSDKOutput(tc.resultText, tc.structured, tc.schema)
			if fallback != tc.wantFallback {
				t.Errorf("fallback = %v, want %v", fallback, tc.wantFallback)
			}
			if tc.wantKey == "" {
				if len(out) != 0 {
					t.Errorf("expected empty output map, got %v", out)
				}
				return
			}
			got, ok := out[tc.wantKey]
			if !ok {
				t.Fatalf("output missing key %q; got %v", tc.wantKey, out)
			}
			if gs, _ := got.(string); gs != tc.wantVal {
				t.Errorf("output[%q] = %v, want %q", tc.wantKey, got, tc.wantVal)
			}
		})
	}
}

// TestParseSDKOutput_NonMapStructured covers the round-trip path for a
// structured_output that is not already a map[string]interface{}.
func TestParseSDKOutput_NonMapStructured(t *testing.T) {
	// json.Unmarshal into `any` yields map[string]interface{} for objects, so
	// simulate a struct-shaped value the SDK might hand back pre-decoded.
	type payload struct {
		Echoed string `json:"echoed"`
	}
	out, _, fallback := parseSDKOutput(nil, payload{Echoed: "STRUCT"}, nil)
	if fallback {
		t.Errorf("fallback = true, want false")
	}
	if out["echoed"] != "STRUCT" {
		t.Errorf("output[echoed] = %v, want STRUCT", out["echoed"])
	}
}
