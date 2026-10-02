package secretguard

import (
	"reflect"
	"strings"
	"testing"
)

func TestMaterializeJSONNeverSplicesText(t *testing.T) {
	swap := func(s string) string { return strings.ReplaceAll(s, "__P__", `v"al`) }
	// A document that does not decode goes through untouched — the tool
	// reports it invalid — never materialised as text.
	if got := string(MaterializeJSON([]byte(`{"a":"__P__"`), swap)); got != `{"a":"__P__"` {
		t.Fatalf("invalid JSON = %q", got)
	}
	if got := string(MaterializeJSON([]byte(`{"a":"x"}`), swap)); got != `{"a":"x"}` {
		t.Fatalf("no placeholder = %q, want the document as is", got)
	}
	if got := string(MaterializeJSON([]byte(`{"a":"__P__","n":1e400,"h":"<b>"}`), swap)); got != `{"a":"v\"al","h":"<b>","n":1e400}` {
		t.Fatalf("materialised = %q", got)
	}
}

func TestMaterializeLeavesReportsAChangeAnywhere(t *testing.T) {
	swap := func(s string) string { return strings.ReplaceAll(s, "__P__", "v") }
	// A placeholder before an unchanged array element, or beside unchanged
	// keys, is a change: one reported from the last leaf alone would hand the
	// tool its placeholder.
	if got := string(MaterializeJSON([]byte(`{"args":["--token","__P__","--verbose"]}`), swap)); got != `{"args":["--token","v","--verbose"]}` {
		t.Fatalf("array = %q", got)
	}
	for range 50 {
		if got := string(MaterializeJSON([]byte(`{"a":"__P__","b":"x","c":"y","d":"z"}`), swap)); got != `{"a":"v","b":"x","c":"y","d":"z"}` {
			t.Fatalf("map = %q", got)
		}
	}
}

func TestRedactLeavesRedactsEveryStringAndNoKey(t *testing.T) {
	red := func(s string) string { return strings.ReplaceAll(s, "SECRET", "__P__") }
	got := RedactLeaves(map[string]any{"SECRET": []any{"a SECRET", map[string]any{"k": "SECRET b"}, 7}}, red)
	want := map[string]any{"SECRET": []any{"a __P__", map[string]any{"k": "__P__ b"}, 7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}
