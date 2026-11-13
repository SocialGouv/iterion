package api

import "testing"

// A message_delta's usage is the provider's account when it names a
// counter, an explicit zero included; an event without one is no report.
func TestParseSSEData_UsageReported(t *testing.T) {
	cases := map[string]bool{
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`:   true,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`:    true,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5}}`:     true,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`:                     false,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":null}}`: false,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`:                                false,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":"n/a"}`:                  false,
	}
	for data, want := range cases {
		ev, err := parseSSEData(data)
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if ev.Usage.Reported != want {
			t.Errorf("%s: Usage %+v, want Reported %v", data, ev.Usage, want)
		}
	}
}
