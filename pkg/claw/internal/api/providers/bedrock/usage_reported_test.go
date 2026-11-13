package bedrock

import "testing"

// Bedrock's Anthropic-shape events report usage like the direct API's.
func TestDecodeAnthropicJSON_UsageReported(t *testing.T) {
	cases := map[string]bool{
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`: true,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`:                   false,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`:                              false,
	}
	for data, want := range cases {
		ev, err := decodeAnthropicJSON([]byte(data))
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if ev.Usage.Reported != want {
			t.Errorf("%s: Usage %+v, want Reported %v", data, ev.Usage, want)
		}
	}
}
