package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// toolEvents decodes events the way a store hands them back: through JSON,
// so numbers are float64 and nested values are plain maps.
func toolEvents(t *testing.T, raw string) []*store.Event {
	t.Helper()
	var events []*store.Event
	if err := json.Unmarshal([]byte(raw), &events); err != nil {
		t.Fatal(err)
	}
	return events
}

// TestInspectNodeNestsAProgramsCallsUnderIt reads the text table: a codemode
// program's nested calls indented under it by the parent id they carry, its
// report as one summary line, a call older than the field flat — and the
// nested calls of two programs sharing one id flat, never assigned to either.
func TestInspectNodeNestsAProgramsCallsUnderIt(t *testing.T) {
	t.Parallel()
	events := toolEvents(t, `[
		{"seq":2,"type":"tool_called","data":{"tool":"glob","tool_use_id":"codemode_A_1","parent_tool_use_id":"toolu_P"}},
		{"seq":3,"type":"tool_error","data":{"tool":"ask_user","tool_use_id":"codemode_A_2","parent_tool_use_id":"toolu_P","error":"pauses the run"}},
		{"seq":4,"type":"tool_error","data":{"tool":"codemode","tool_use_id":"toolu_P","error":"refused","codemode":{
			"limits":{"tool_calls":4096},"calls":2,"budget_refused":1,"refusal":"script_error",
			"adjusted":[{"field":"tool_calls","requested":99999,"applied":4096}],"source_bytes":9}}},
		{"seq":5,"type":"tool_called","data":{"tool":"Bash"}},
		{"seq":6,"type":"tool_called","data":{"tool":"codemode","tool_use_id":"call_0"}},
		{"seq":7,"type":"tool_called","data":{"tool":"grep","tool_use_id":"codemode_B_1","parent_tool_use_id":"call_0"}},
		{"seq":8,"type":"tool_called","data":{"tool":"codemode","tool_use_id":"call_0"}}
	]`)
	var buf bytes.Buffer
	renderNodeReport(&nodeReport{NodeID: "n", Tools: buildToolCalls(events)}, &Printer{Format: OutputHuman, W: &buf})
	out := buf.String()
	table := out[strings.Index(out, "Tool calls"):]
	var lines []string
	for _, l := range strings.Split(table, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] != "Tool" && f[0] != "SEQ" && !strings.HasPrefix(f[0], "─") {
			lines = append(lines, strings.Join(f, " "))
		}
	}
	want := []string{
		"4 codemode error —",
		"program calls 2/4096 · 1 refused on budget · refused: script_error · adjusted: tool_calls 99999→4096",
		"2 └ glob ok —",
		"3 └ ask_user error —",
		"5 Bash ok —",
		"6 codemode ok —",
		"7 grep ok —",
		"8 codemode ok —",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("tool table rows:\n%s\nwant:\n%s\n(full output:\n%s)", strings.Join(lines, "\n"), strings.Join(want, "\n"), out)
	}
}

// TestInspectNodeShowsEveryCallOfACycleAndAnyDepth: no row is lost to the
// nesting — a call two levels down shows under both of its ancestors, the
// launcher's own refusal shows marked under the parent it names, and every
// call of a forged cycle shows at the top.
func TestInspectNodeShowsEveryCallOfACycleAndAnyDepth(t *testing.T) {
	t.Parallel()
	events := toolEvents(t, `[
		{"seq":1,"type":"tool_called","data":{"tool":"codemode","tool_use_id":"P"}},
		{"seq":2,"type":"tool_called","data":{"tool":"agent","tool_use_id":"Q","parent_tool_use_id":"P"}},
		{"seq":3,"type":"tool_called","data":{"tool":"glob","tool_use_id":"R","parent_tool_use_id":"Q"}},
		{"seq":4,"type":"tool_error","data":{"tool":"mcp_x","tool_use_id":"P","parent_tool_use_id":"Q","emitted_by":"launcher","error":"denied"}},
		{"seq":5,"type":"tool_called","data":{"tool":"c","tool_use_id":"M","parent_tool_use_id":"N"}},
		{"seq":6,"type":"tool_called","data":{"tool":"d","tool_use_id":"N","parent_tool_use_id":"M"}}
	]`)
	var buf bytes.Buffer
	renderNodeReport(&nodeReport{NodeID: "n", Tools: buildToolCalls(events)}, &Printer{Format: OutputHuman, W: &buf})
	out := buf.String()
	table := out[strings.Index(out, "Tool calls"):]
	var lines []string
	for _, l := range strings.Split(table, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] != "Tool" && f[0] != "SEQ" && !strings.HasPrefix(f[0], "─") {
			lines = append(lines, strings.TrimRight(l, " "))
		}
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"  1    codemode",
		"  2      └ agent",
		"  3        └ glob",
		"  4        └ mcp_x [launcher]",
		"  5    c",
		"  6    d",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("row %q missing from:\n%s", want, got)
		}
	}
	if len(lines) != 6 {
		t.Errorf("%d rows for 6 calls:\n%s", len(lines), got)
	}
}
