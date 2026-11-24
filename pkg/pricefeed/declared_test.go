package pricefeed

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func declaredParams(entries string) map[string]string {
	return map[string]string{"entries": entries}
}

// A declared feed is a static source: its answer is its params, read
// without a fetch, and the registry names the type.
func TestDeclaredSourceIsStatic(t *testing.T) {
	src, ok := SourceFor(TypeDeclared)
	if !ok {
		t.Fatalf("%q is not registered (registered: %v)", TypeDeclared, SourceTypes())
	}
	static, ok := src.(StaticSource)
	if !ok {
		t.Fatalf("the declared source does not answer from its params (%T is not a StaticSource)", src)
	}
	params := declaredParams(`{"Scaleway/DeepSeek-X":{"reasoning_efforts":["low","high","xhigh","max"]},"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`)
	entries, err := static.Entries(params)
	if err != nil {
		t.Fatalf("a valid declaration refused: %v", err)
	}
	if got := entries["scaleway/deepseek-x"].ReasoningEfforts; !reflect.DeepEqual(got, []string{"low", "high", "xhigh", "max"}) {
		t.Fatalf("declared efforts read %v", got)
	}
	if got := entries["glm-5.3"]; got.ContextWindow != 256000 || got.MaxOutputTokens != 16384 || got.InputCostPerM != 2.01 || got.OutputCostPerM != 6.15 {
		t.Fatalf("declared entry read %+v", got)
	}
	fetched, err := src.Fetch(context.Background(), params)
	if err != nil || !reflect.DeepEqual(fetched, entries) {
		t.Fatalf("Fetch answered %+v %v, want its params' entries %+v", fetched, err, entries)
	}
	if !slices.Contains(SourceTypes(), TypeDeclared) {
		t.Fatalf("SourceTypes omits %q: %v", TypeDeclared, SourceTypes())
	}
}

// What a declaration may state: a priced entry, an efforts-only entry,
// a window, an output cap, a cache price or a capability flag with or
// without the price pair — whatever an operator-table row states — or
// any of them together. What it may not: an empty document, or an entry
// that states nothing; the refusal names the id.
func TestDeclaredValidateParams(t *testing.T) {
	src, _ := SourceFor(TypeDeclared)
	for _, ok := range []string{
		`{"glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`,
		`{"deepseek-x":{"reasoning_efforts":["high"]}}`,
		`{"deepseek-x":{"context_window":128000,"cache_read_usd_per_mtok":0.1,"input_usd_per_mtok":1,"output_usd_per_mtok":2,"tool_call":true,"reasoning_efforts":["low","max"]}}`,
	} {
		if err := src.ValidateParams(declaredParams(ok)); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	// A pool's OPENAI_COMPATIBLE_MODELS row may state a field without the
	// price pair; declared as written, it answers whole (Fold).
	for _, unpriced := range []struct{ field, raw string }{
		{"context_window", `{"deepseek-x":{"context_window":128000}}`},
		{"max_output_tokens", `{"deepseek-x":{"max_output_tokens":8192,"reasoning_efforts":["high"]}}`},
		{"cache_read_usd_per_mtok", `{"deepseek-x":{"cache_read_usd_per_mtok":0.1}}`},
		{"reasoning", `{"deepseek-x":{"reasoning":true}}`},
		{"tool_call", `{"deepseek-x":{"tool_call":false}}`},
		{"temperature", `{"deepseek-x":{"temperature":true}}`},
	} {
		if err := src.ValidateParams(declaredParams(unpriced.raw)); err != nil {
			t.Errorf("%s without the price pair refused: %v", unpriced.field, err)
		}
	}
	for _, empty := range []string{"", "  ", "{}"} {
		if err := src.ValidateParams(declaredParams(empty)); err == nil || !strings.Contains(err.Error(), "entries") {
			t.Errorf("an empty declaration %q validated or refused without naming params.entries: %v", empty, err)
		}
	}
	if err := src.ValidateParams(map[string]string{}); err == nil {
		t.Error("a declaration with no entries param validated")
	}
	for _, nothing := range []string{`{"deepseek-x":{}}`, `{"deepseek-x":null}`} {
		if err := src.ValidateParams(declaredParams(nothing)); err == nil || !strings.Contains(err.Error(), `"deepseek-x"`) || !strings.Contains(err.Error(), "states nothing") {
			t.Errorf("%s validated or refused without naming the id: %v", nothing, err)
		}
	}
	if err := src.ValidateParams(declaredParams(`{"deepseek-x":{"reasoning_efforts":["hihg"]}}`)); err == nil || !strings.Contains(err.Error(), "params.entries") {
		t.Errorf("an invalid effort level validated or refused without naming params.entries: %v", err)
	}
	// The same rules through the whole-feed validation a write route runs.
	feed := FeedConfig{Name: "pliage", Type: TypeDeclared, Enabled: true, Interval: d(30 * 86400), Timeout: d(1),
		Params: declaredParams(`{"deepseek-x":{"context_window":128000}}`)}
	if err := ValidateFeed(feed); err != nil {
		t.Errorf("ValidateFeed refused an unpriced declared window: %v", err)
	}
	feed.Params = declaredParams(`{"deepseek-x":{}}`)
	if err := ValidateFeed(feed); err == nil || !strings.Contains(err.Error(), `feed "pliage"`) {
		t.Errorf("ValidateFeed accepted an entry stating nothing: %v", err)
	}
}

// The fold reads a declaration from its config, so its Entries answer
// holds the write rules too: a stored document that breaks them (written
// past the route) refuses instead of answering.
func TestDeclaredEntriesHoldTheWriteRules(t *testing.T) {
	src, _ := SourceFor(TypeDeclared)
	static := src.(StaticSource)
	for _, bad := range []string{`{}`, `{"deepseek-x":{}}`} {
		if got, err := static.Entries(declaredParams(bad)); err == nil {
			t.Errorf("Entries(%s) answered %+v — a declaration the write rules refuse must not answer", bad, got)
		}
	}
}

// The reconciler never fetches a static feed: its answer is its config,
// read at every fold, so no refresh is ever due — with or without a
// state document.
func TestDueNeverFetchesAStaticFeed(t *testing.T) {
	now := time.Now()
	feed := FeedConfig{Name: "pliage", Type: TypeDeclared, Enabled: true, Interval: d(30 * 86400), Timeout: d(1),
		Params: declaredParams(`{"glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`)}
	if Due(feed, nil, now) {
		t.Fatal("a declared feed with no state is due — the reconciler would fetch a declaration")
	}
	if Due(feed, &StateRecord{LastAttempt: now.Add(-365 * 24 * time.Hour), LastError: "boom"}, now) {
		t.Fatal("a declared feed with an old failing state is due")
	}
	fetched := feed
	fetched.Type = TypeExec
	if !Due(fetched, nil, now) {
		t.Fatal("a fetched feed with no state is not due")
	}
}
