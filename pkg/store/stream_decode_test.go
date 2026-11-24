package store

import (
	"encoding/json"
	"testing"
)

// decodeStreamEventJSON reads one format-one line: anything after the record
// but whitespace is refused; numbers keep the literal they were written with.
func TestDecodeStreamEventJSONLegacyLines(t *testing.T) {
	const record = `{"seq":1,"type":"node_finished","run_id":"r"}`
	for _, tc := range []struct {
		name string
		line string
		ok   bool
	}{
		{"plain", record, true},
		{"trailing spaces", record + "   ", true},
		{"trailing carriage return", record + "\r", true},
		{"trailing tab carriage return space", record + "\t\r ", true},
		{"leading spaces", "   " + record, true},
		{"glued record", record + `{"seq":2}`, false},
		{"second record", record + ` {"seq":2}`, false},
		{"trailing number", record + ` 7`, false},
		{"trailing bracket", record + `]`, false},
		{"trailing brace", record + `}`, false},
		{"trailing comma", record + `,`, false},
		{"trailing word", record + `x`, false},
		{"truncated", record[:len(record)-1], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var evt Event
			err := decodeStreamEventJSON(t.Context(), LegacyValueFormat, []byte(tc.line), &evt)
			if tc.ok && (err != nil || evt.Seq != 1 || evt.Type != EventNodeFinished) {
				t.Fatalf("decode %q = %v, %+v", tc.line, err, evt)
			}
			if !tc.ok && err == nil {
				t.Fatalf("decode %q accepted: %+v", tc.line, evt)
			}
		})
	}
	for _, tc := range []struct {
		name, data string
	}{
		{"past 2^53", `{"v":9007199254740993}`},
		{"past float64", `{"v":1e400}`},
		{"negative zero", `{"v":-0.0}`},
		{"upper exponent", `{"v":1E5}`},
		{"nested", `{"a":[1e400,{"b":9007199254740993,"c":[[-0.0]]}],"d":1E5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := `{"seq":1,"type":"node_finished","run_id":"r","data":` + tc.data + `}`
			var evt Event
			if err := decodeStreamEventJSON(t.Context(), LegacyValueFormat, []byte(line), &evt); err != nil {
				t.Fatalf("decode %s: %v", line, err)
			}
			if v, present := evt.Data["v"]; present {
				if _, isNumber := v.(json.Number); !isNumber {
					t.Fatalf("data.v decoded as %T, want json.Number", v)
				}
			}
			wire, err := json.Marshal(evt.Data)
			if err != nil {
				t.Fatal(err)
			}
			if string(wire) != tc.data {
				t.Fatalf("data re-encoded as %s, want %s", wire, tc.data)
			}
		})
	}
}
