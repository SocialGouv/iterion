package privacy

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy/detector"
)

// fakeRegistry is a minimal in-memory registry used by tests so we
// don't pull in the full *tool.Registry. Only the
// `RegisterBuiltin` method is exercised.
type fakeRegistry struct {
	mu    sync.Mutex
	items map[string]registered
}

type registered struct {
	desc   string
	schema json.RawMessage
	exec   func(ctx context.Context, input json.RawMessage) (string, error)
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{items: map[string]registered{}}
}

func (r *fakeRegistry) RegisterBuiltin(name, desc string, schema json.RawMessage, exec func(ctx context.Context, input json.RawMessage) (string, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[name] = registered{desc: desc, schema: schema, exec: exec}
	return nil
}

func (r *fakeRegistry) get(name string) (registered, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.items[name]
	return v, ok
}

func newTestConfig(t *testing.T) (*Config, context.Context) {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		StoreDir:     dir,
		Detector:     detector.New(),
		RunIDFromCtx: func(ctx context.Context) string { return "test-run-1" },
	}
	return cfg, context.Background()
}

func TestRegister_BothToolsRegistered(t *testing.T) {
	cfg, _ := newTestConfig(t)
	r := newFakeRegistry()
	if err := RegisterFilter(r, cfg); err != nil {
		t.Fatalf("RegisterFilter: %v", err)
	}
	if err := RegisterUnfilter(r, cfg); err != nil {
		t.Fatalf("RegisterUnfilter: %v", err)
	}
	if _, ok := r.get(filterToolName); !ok {
		t.Fatalf("privacy_filter not registered")
	}
	if _, ok := r.get(unfilterToolName); !ok {
		t.Fatalf("privacy_unfilter not registered")
	}
}

func TestRegister_NilConfigRejected(t *testing.T) {
	r := newFakeRegistry()
	if err := RegisterFilter(r, nil); err == nil {
		t.Fatalf("expected error for nil config")
	}
	if err := RegisterFilter(r, &Config{StoreDir: "x"}); err == nil {
		t.Fatalf("expected error for missing detector")
	}
}

func runFilter(t *testing.T, r *fakeRegistry, ctx context.Context, in any) map[string]any {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	got, ok := r.get(filterToolName)
	if !ok {
		t.Fatal("filter not registered")
	}
	out, err := got.exec(ctx, body)
	if err != nil {
		t.Fatalf("filter exec: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out)
	}
	return m
}

func runUnfilter(t *testing.T, r *fakeRegistry, ctx context.Context, in any) (map[string]any, error) {
	t.Helper()
	body, _ := json.Marshal(in)
	got, _ := r.get(unfilterToolName)
	out, err := got.exec(ctx, body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	return m, nil
}

func registerBoth(t *testing.T, cfg *Config) *fakeRegistry {
	t.Helper()
	r := newFakeRegistry()
	if err := RegisterFilter(r, cfg); err != nil {
		t.Fatalf("RegisterFilter: %v", err)
	}
	if err := RegisterUnfilter(r, cfg); err != nil {
		t.Fatalf("RegisterUnfilter: %v", err)
	}
	return r
}

func TestRedact_RoundTrip(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)

	src := "Email me at alice@example.com please"
	red := runFilter(t, r, ctx, map[string]any{"text": src})
	redacted := red["redacted"].(string)
	if strings.Contains(redacted, "alice@example.com") {
		t.Fatalf("redacted output still contains email: %q", redacted)
	}
	if !strings.Contains(redacted, "PII_") {
		t.Fatalf("redacted output missing PII token: %q", redacted)
	}

	res, err := runUnfilter(t, r, ctx, map[string]any{"text": redacted})
	if err != nil {
		t.Fatalf("unfilter: %v", err)
	}
	if res["text"].(string) != src {
		t.Fatalf("round trip: got %q, want %q", res["text"], src)
	}
}

func TestRedact_TokenStability(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	src := "alice@example.com"
	a := runFilter(t, r, ctx, map[string]any{"text": src})
	b := runFilter(t, r, ctx, map[string]any{"text": src})
	if a["redacted"].(string) != b["redacted"].(string) {
		t.Fatalf("token instability: a=%q b=%q", a["redacted"], b["redacted"])
	}
}

func TestRedact_TokenDifferentByRun(t *testing.T) {
	cfg1, _ := newTestConfig(t)
	cfg2, _ := newTestConfig(t)
	cfg2.RunIDFromCtx = func(ctx context.Context) string { return "different-run" }
	cfg2.StoreDir = cfg1.StoreDir // same store; only runID differs
	r1 := registerBoth(t, cfg1)
	r2 := registerBoth(t, cfg2)

	src := "alice@example.com"
	a := runFilter(t, r1, context.Background(), map[string]any{"text": src})
	b := runFilter(t, r2, context.Background(), map[string]any{"text": src})
	if a["redacted"].(string) == b["redacted"].(string) {
		t.Fatalf("tokens should differ by run, both = %q", a["redacted"])
	}
}

func TestRedact_HasCategoryBooleans(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	out := runFilter(t, r, ctx, map[string]any{"text": "alice@example.com"})
	for _, cat := range allCategories {
		key := "has_" + cat
		v, ok := out[key]
		if !ok {
			t.Fatalf("missing %q in output", key)
		}
		if cat == "email" {
			if !v.(bool) {
				t.Fatalf("has_email = false, want true")
			}
		} else {
			if v.(bool) {
				t.Fatalf("has_%s = true, want false", cat)
			}
		}
	}
}

func TestDetect_NoRawValues(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	out := runFilter(t, r, ctx, map[string]any{
		"text": "alice@example.com",
		"mode": "detect",
	})
	spans := out["spans"].([]any)
	if len(spans) == 0 {
		t.Fatalf("expected ≥1 span")
	}
	first := spans[0].(map[string]any)
	if _, leaked := first["value"]; leaked {
		t.Fatalf("detect output leaked raw value: %+v", first)
	}
	hash := first["value_hash"].(string)
	if len(hash) != 32 {
		t.Fatalf("value_hash len = %d, want 32", len(hash))
	}
}

func TestUnfilter_MissingLeave(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	res, err := runUnfilter(t, r, ctx, map[string]any{
		"text": "Hello PII_deadbeef",
	})
	if err != nil {
		t.Fatalf("unfilter: %v", err)
	}
	if !strings.Contains(res["text"].(string), "PII_deadbeef") {
		t.Fatalf("missing token should be left in place: %q", res["text"])
	}
	if len(res["missing"].([]any)) != 1 {
		t.Fatalf("expected 1 missing, got %v", res["missing"])
	}
}

func TestUnfilter_MissingRemove(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	res, err := runUnfilter(t, r, ctx, map[string]any{
		"text":           "Hello PII_deadbeef!",
		"missing_policy": "remove",
	})
	if err != nil {
		t.Fatalf("unfilter: %v", err)
	}
	if strings.Contains(res["text"].(string), "PII_deadbeef") {
		t.Fatalf("missing token should be removed: %q", res["text"])
	}
}

func TestUnfilter_MissingError(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	_, err := runUnfilter(t, r, ctx, map[string]any{
		"text":           "Hello PII_deadbeef!",
		"missing_policy": "error",
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// recordingDetector wraps a real detector and counts Scan calls
// so we can assert short-circuit on empty input.
type recordingDetector struct {
	calls int
}

// Wrap calls into the detector's exposed Scan signature by
// embedding a real detector and intercepting Scan via a private
// method. To stay simple we instead use a custom config that
// substitutes a single-rule detector — empty text MUST short
// circuit before reaching any rule.

func TestEmptyText_NoCallToDetector(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	wrap := detector.NewWithRules([]detector.Rule{
		&countingRule{
			callsRef: &calls,
		},
	})
	cfg := &Config{
		StoreDir:     dir,
		Detector:     wrap,
		RunIDFromCtx: func(ctx context.Context) string { return "run-1" },
	}
	r := registerBoth(t, cfg)
	out := runFilter(t, r, context.Background(), map[string]any{"text": "   \n\t  "})
	if out["mode"].(string) != "redact" {
		t.Fatalf("mode mismatch")
	}
	if calls != 0 {
		t.Fatalf("expected 0 detector calls on whitespace, got %d", calls)
	}
}

type countingRule struct {
	callsRef *int
}

func (c *countingRule) Name() string     { return "counter" }
func (c *countingRule) Category() string { return "secret" }
func (c *countingRule) Find(text string, byteToRune []int) []detector.Span {
	*c.callsRef++
	return nil
}

func TestNoRunIDInContext(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		StoreDir:     dir,
		Detector:     detector.New(),
		RunIDFromCtx: func(ctx context.Context) string { return "" },
	}
	r := registerBoth(t, cfg)
	body, _ := json.Marshal(map[string]any{"text": "alice@example.com"})
	got, _ := r.get(filterToolName)
	if _, err := got.exec(context.Background(), body); err == nil {
		t.Fatalf("expected error when no runID in context")
	}
}

func TestCustomPlaceholderFormat(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	out := runFilter(t, r, ctx, map[string]any{
		"text":               "alice@example.com",
		"placeholder_format": "<<{category}:{token}>>",
	})
	red := out["redacted"].(string)
	if !strings.Contains(red, "<<EMAIL:PII_") {
		t.Fatalf("custom format missing in output: %q", red)
	}
}

func TestRuneIndices_Emoji(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	src := "😀 ping alice@example.com! 😀"
	red := runFilter(t, r, ctx, map[string]any{"text": src})
	res, err := runUnfilter(t, r, ctx, map[string]any{"text": red["redacted"]})
	if err != nil {
		t.Fatalf("unfilter: %v", err)
	}
	if res["text"].(string) != src {
		t.Fatalf("emoji round-trip failed: %q != %q", res["text"], src)
	}
}

func TestRedact_DuplicateValuesShareToken(t *testing.T) {
	cfg, ctx := newTestConfig(t)
	r := registerBoth(t, cfg)
	src := "alice@example.com or alice@example.com again"
	out := runFilter(t, r, ctx, map[string]any{"text": src})
	red := out["redacted"].(string)
	// Both occurrences should map to the same placeholder.
	count := strings.Count(red, "PII_")
	if count != 2 {
		t.Fatalf("expected 2 placeholder occurrences in redacted, got %d (%q)", count, red)
	}
	placeholders := out["placeholders"].([]any)
	if len(placeholders) != 1 {
		t.Fatalf("expected 1 unique placeholder entry, got %d (%v)", len(placeholders), placeholders)
	}
}
