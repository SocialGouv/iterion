package supervise

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSpecWithDefaults(t *testing.T) {
	cases := []struct {
		name         string
		in           Spec
		wantCooldown time.Duration
		wantMax      int
	}{
		{"zero values filled", Spec{}, DefaultCooldown, DefaultMaxEvals},
		{"negative values filled", Spec{Cooldown: -time.Second, MaxEvals: -3}, DefaultCooldown, DefaultMaxEvals},
		{"explicit values preserved", Spec{Cooldown: 5 * time.Second, MaxEvals: 7}, 5 * time.Second, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.Cooldown != tc.wantCooldown {
				t.Errorf("Cooldown = %v; want %v", got.Cooldown, tc.wantCooldown)
			}
			if got.MaxEvals != tc.wantMax {
				t.Errorf("MaxEvals = %d; want %d", got.MaxEvals, tc.wantMax)
			}
		})
	}
}

func TestSpecWatchesNode(t *testing.T) {
	cases := []struct {
		name    string
		watches []string
		node    string
		want    bool
	}{
		{"empty watches = run scope, any node", nil, "anything", true},
		{"empty watches, empty node", nil, "", true},
		{"listed node", []string{"a", "b"}, "b", true},
		{"unlisted node", []string{"a", "b"}, "c", false},
		{"empty node vs non-empty watches", []string{"a"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Spec{Watches: tc.watches}
			if got := s.watchesNode(tc.node); got != tc.want {
				t.Errorf("watchesNode(%q) = %v; want %v", tc.node, got, tc.want)
			}
		})
	}
}

func TestMonitorMatches(t *testing.T) {
	toolErr := ev(1, store.EventToolError, "impl", map[string]any{"tool": "Bash", "error": "exit 1"})
	cases := []struct {
		name string
		m    Monitor
		evt  *store.Event
		want bool
	}{
		{"nil event never matches", Monitor{EventType: "tool_error"}, nil, false},
		{"empty monitor never matches (no wildcard-everything)", Monitor{}, toolErr, false},
		{"event type match", Monitor{EventType: "tool_error"}, toolErr, true},
		{"event type mismatch", Monitor{EventType: "node_finished"}, toolErr, false},
		{"node id match", Monitor{NodeID: "impl"}, toolErr, true},
		{"node id mismatch", Monitor{NodeID: "other"}, toolErr, false},
		{"tool name case-insensitive", Monitor{ToolName: "bash"}, toolErr, true},
		{"tool name mismatch", Monitor{ToolName: "Edit"}, toolErr, false},
		{"tool_name data key honoured", Monitor{ToolName: "Bash"},
			ev(2, store.EventToolCalled, "impl", map[string]any{"tool_name": "Bash"}), true},
		{"text contains case-insensitive over rendered event", Monitor{TextContains: "EXIT 1"}, toolErr, true},
		{"text contains matches node id in rendering", Monitor{TextContains: "node=impl"}, toolErr, true},
		{"text contains miss", Monitor{TextContains: "nothing here"}, toolErr, false},
		{"all set fields must match (conjunction)", Monitor{EventType: "tool_error", ToolName: "Edit"}, toolErr, false},
		{"conjunction all matching", Monitor{EventType: "tool_error", NodeID: "impl", ToolName: "Bash", TextContains: "exit"}, toolErr, true},
		{"cost_gt fires above threshold", Monitor{CostGt: 10},
			ev(3, store.EventBudgetWarning, "", map[string]any{"used": 15.0}), true},
		{"cost_gt exact threshold does not fire", Monitor{CostGt: 10},
			ev(4, store.EventBudgetWarning, "", map[string]any{"used": 10.0}), false},
		{"cost_gt tolerates int used", Monitor{CostGt: 10},
			ev(5, store.EventBudgetWarning, "", map[string]any{"used": 11}), true},
		{"cost_gt requires budget_warning event", Monitor{CostGt: 10},
			ev(6, store.EventToolError, "", map[string]any{"used": 99.0}), false},
		{"cost_gt without used field", Monitor{CostGt: 10},
			ev(7, store.EventBudgetWarning, "", nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.matches(tc.evt); got != tc.want {
				t.Errorf("matches = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestMonitorIsEmpty(t *testing.T) {
	if !(Monitor{}).isEmpty() {
		t.Error("zero monitor should be empty")
	}
	for name, m := range map[string]Monitor{
		"event_type":    {EventType: "x"},
		"node_id":       {NodeID: "x"},
		"tool_name":     {ToolName: "x"},
		"text_contains": {TextContains: "x"},
		"cost_gt":       {CostGt: 1},
	} {
		if m.isEmpty() {
			t.Errorf("monitor with %s set reported empty", name)
		}
	}
}

func TestEventToolName(t *testing.T) {
	cases := []struct {
		name string
		evt  *store.Event
		want string
	}{
		{"nil data", ev(1, store.EventToolCalled, "", nil), ""},
		{"tool key", ev(2, store.EventToolCalled, "", map[string]any{"tool": "Bash"}), "Bash"},
		{"tool_name key", ev(3, store.EventToolCalled, "", map[string]any{"tool_name": "Edit"}), "Edit"},
		{"tool wins over tool_name", ev(4, store.EventToolCalled, "", map[string]any{"tool": "A", "tool_name": "B"}), "A"},
		{"empty tool falls through to tool_name", ev(5, store.EventToolCalled, "", map[string]any{"tool": "", "tool_name": "B"}), "B"},
		{"non-string ignored", ev(6, store.EventToolCalled, "", map[string]any{"tool": 42}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventToolName(tc.evt); got != tc.want {
				t.Errorf("eventToolName = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestNumField(t *testing.T) {
	cases := []struct {
		name   string
		data   map[string]any
		want   float64
		wantOK bool
	}{
		{"nil map", nil, 0, false},
		{"missing key", map[string]any{}, 0, false},
		{"float64", map[string]any{"used": 1.5}, 1.5, true},
		{"float32", map[string]any{"used": float32(2)}, 2, true},
		{"int", map[string]any{"used": 3}, 3, true},
		{"int64", map[string]any{"used": int64(4)}, 4, true},
		{"string not numeric", map[string]any{"used": "5"}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := numField(tc.data, "used")
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("numField = (%v, %v); want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRenderEvent(t *testing.T) {
	if got := RenderEvent(nil); got != "" {
		t.Errorf("RenderEvent(nil) = %q; want empty", got)
	}
	plain := ev(7, store.EventNodeStarted, "", nil)
	if got := RenderEvent(plain); got != "#7 node_started" {
		t.Errorf("RenderEvent = %q; want %q", got, "#7 node_started")
	}
	full := ev(8, store.EventToolError, "impl", map[string]any{"tool": "Bash"})
	got := RenderEvent(full)
	if !strings.HasPrefix(got, "#8 tool_error node=impl ") {
		t.Errorf("RenderEvent = %q; want prefix %q", got, "#8 tool_error node=impl ")
	}
	if !strings.Contains(got, `"tool":"Bash"`) {
		t.Errorf("RenderEvent = %q; want JSON data with tool", got)
	}
}

func TestIsTurnBoundary(t *testing.T) {
	boundary := []store.EventType{store.EventLLMStepFinished, store.EventNodeFinished, store.EventNodeStarted, store.EventRunPaused}
	for _, typ := range boundary {
		if !IsTurnBoundary(ev(1, typ, "", nil)) {
			t.Errorf("IsTurnBoundary(%s) = false; want true", typ)
		}
	}
	notBoundary := []store.EventType{store.EventToolCalled, store.EventToolError, store.EventRunFinished, store.EventBudgetWarning}
	for _, typ := range notBoundary {
		if IsTurnBoundary(ev(1, typ, "", nil)) {
			t.Errorf("IsTurnBoundary(%s) = true; want false", typ)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []store.EventType{store.EventRunFinished, store.EventRunFailed, store.EventRunCancelled}
	for _, typ := range terminal {
		if !IsTerminal(ev(1, typ, "", nil)) {
			t.Errorf("IsTerminal(%s) = false; want true", typ)
		}
	}
	notTerminal := []store.EventType{store.EventRunPaused, store.EventNodeFinished, store.EventRunStarted}
	for _, typ := range notTerminal {
		if IsTerminal(ev(1, typ, "", nil)) {
			t.Errorf("IsTerminal(%s) = true; want false", typ)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("exact", 5); got != "exact" {
		t.Errorf("truncate exact-length = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q; want %q", got, "abc…")
	}
}

func TestFormatOperatorMessages(t *testing.T) {
	if got := FormatOperatorMessages(nil); got != "" {
		t.Errorf("empty slice = %q; want empty string", got)
	}
	one := FormatOperatorMessages([]string{"fix it"})
	if one != "Operator queued message:\n\nfix it" {
		t.Errorf("single = %q", one)
	}
	two := FormatOperatorMessages([]string{"a", "b"})
	if two != "Operator queued messages:\n\na\n---\nb" {
		t.Errorf("plural = %q", two)
	}
}
