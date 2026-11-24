package modelspecs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The one effort vocabulary: the named levels and the integers 1..100,
// trimmed and lower-cased, numbers accepted as JSON numbers or strings.
// The env reader (compatgw) and the feed parser (pricefeed) both read
// through it, so a level one accepts the other accepts.
func TestParseEffortLevelsVocabulary(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want []string
	}{
		{`["none","minimal","low","medium","high","xhigh","max"]`, []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{`["  HIGH ","Max"]`, []string{"high", "max"}},
		{`[1, 50, 100]`, []string{"1", "50", "100"}},
		{`["007"," 42 "]`, []string{"7", "42"}},
		// Strictness about an empty list or a repeated level belongs to
		// the feed parser; the vocabulary leaf answers what it read.
		{`[]`, []string{}},
		{`["low","low"]`, []string{"low", "low"}},
	} {
		got, err := ParseEffortLevels(json.RawMessage(c.raw))
		if err != nil {
			t.Errorf("%s refused: %v", c.raw, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s parsed %#v, want %#v", c.raw, got, c.want)
		}
	}
	for _, c := range []struct {
		raw  string
		want string
	}{
		{`["hihg"]`, `"hihg"`},
		{`[0]`, `"0"`},
		{`[101]`, `"101"`},
		{`["low", true]`, `"true"`},
		{`null`, "expected an array"},
		{`{"low":1}`, "expected an array"},
		{`"high"`, "expected an array"},
	} {
		if got, err := ParseEffortLevels(json.RawMessage(c.raw)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s answered %v, %v — want a refusal naming %s", c.raw, got, err, c.want)
		}
	}
}
