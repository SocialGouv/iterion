package parser

import (
	"encoding/json"
	"strings"
	"testing"
)

// The JSON the text can write: what the lexer reads back. A value the
// transport accepts and the text cannot write is refused here, naming the
// rule, so a document never becomes unsaveable in silence.
func TestWritableJSONValueRefusesWhatTheTextCannotWrite(t *testing.T) {
	for _, raw := range []string{`-1`, `1e-3`, `1E10`, `{"a": -2}`, `[1, [2, -3]]`, `{"k": {"n": 2e5}}`, `-0.5`} {
		err := WritableJSONValue(json.RawMessage(raw))
		if err == nil {
			t.Errorf("%s: accepted, though the text cannot write it", raw)
			continue
		}
		if !strings.Contains(err.Error(), "no signed number and no exponent") {
			t.Errorf("%s: the refusal does not name the rule: %v", raw, err)
		}
	}
	for _, raw := range []string{`1`, `1.5`, `0`, `"x"`, `"-1"`, `null`, `true`, `[]`, `{}`, `{"k": [true, 1, "x"]}`, `[1.25, {"n": 3}]`} {
		if err := WritableJSONValue(json.RawMessage(raw)); err != nil {
			t.Errorf("%s: refused, though the text writes it: %v", raw, err)
		}
	}
	if err := WritableJSONValue(nil); err != nil {
		t.Errorf("an absent value is writable: %v", err)
	}
	// Names the path of the offending number inside a structure.
	if err := WritableJSONValue(json.RawMessage(`{"outer": {"inner": [0, -1]}}`)); err == nil || !strings.Contains(err.Error(), ".outer.inner[1]") {
		t.Errorf("the offending number is not located: %v", err)
	}
	// The decoder's own refusals travel: duplicate keys, trailing values.
	for _, raw := range []string{`{"a": 1, "a": 2}`, `1 2`} {
		if err := WritableJSONValue(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted, though the decoder refuses it", raw)
		}
	}
}
