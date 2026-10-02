package ir

import (
	"fmt"
	"strings"
	"testing"
)

func emptyToolsBotWith(backend, fallback, extra string) string {
	return strings.Replace(emptyToolsBot(backend, fallback), "  tools: []\n", "  tools: []\n"+extra, 1)
}

func emptyToolsBot(backend, fallback string) string {
	fb := ""
	if fallback != "" {
		fb = fmt.Sprintf("  fallbacks:\n    fb:\n      backend: %q\n", fallback)
	}
	return fmt.Sprintf(`prompt sys:
  """s"""

prompt usr:
  """u"""

judge reviewer:
  model: "openai/gpt-5.5"
  backend: %q
  system: sys
  user: usr
  tools: []
%s
workflow w:
  entry: reviewer
  reviewer -> done
`, backend, fb)
}

// A declared-empty `tools:` is a real bound on the three backends that
// receive the list, and dropped on the three driven through the CLI-agent
// seam. C270 is what tells the author which one they wrote.
//
// Reddens on the mutation that makes ReceivesToolList answer true
// for every backend (the forbidden alternative: a bound assumed universal).
func TestC270FiresOnlyWhereTheBackendNeverReceivesTheToolList(t *testing.T) {
	for _, tc := range []struct {
		backend string
		want    bool
	}{
		{"claw", false},
		{"claude_code", false},
		{"codex", false},
		{"pi", true},
		{"kimi", true},
		{"grok", true},
	} {
		r := compileSrc(t, emptyToolsBot(tc.backend, ""))
		if tc.want {
			expectDiag(t, r, DiagEmptyToolsNotEnforced)
		} else {
			expectNoDiag(t, r, DiagEmptyToolsNotEnforced)
		}
	}
}

// The same bound, dropped by a fallback ROUTE: reported against the route,
// because a chain exists to serve when the primary is already failing.
func TestC270FiresOnAFallbackRouteThatDropsTheDeclaredEmptyToolList(t *testing.T) {
	r := compileSrc(t, emptyToolsBot("claw", "pi"))
	expectDiag(t, r, DiagEmptyToolsNotEnforced)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagEmptyToolsNotEnforced {
			msg = d.Message
		}
	}
	if !strings.Contains(msg, "fallback") {
		t.Errorf("the route must be named in the message, got %q", msg)
	}
	expectNoDiag(t, compileSrc(t, emptyToolsBot("claw", "claude_code")), DiagEmptyToolsNotEnforced)
}

// An UNDECLARED list must never draw C270: the diagnostic is about a bound
// that was written and is being dropped, not about a node with no bound.
func TestC270IsSilentOnAnUndeclaredToolList(t *testing.T) {
	src := strings.Replace(emptyToolsBot("pi", ""), "  tools: []\n", "", 1)
	expectNoDiag(t, compileSrc(t, src), DiagEmptyToolsNotEnforced)
}

// The other way a declared-empty list stops holding: the runtime RE-POPULATES
// it. On claw `interaction:` grants `ask_user` whatever the list says (claw
// carries it through the tool loop), and that one entry unlocks `todo_write`
// and, under `auto_memory:`, `read_file`/`write_file`/`glob` — measured:
// `[ask_user todo_write read_file write_file glob]` on a node that declared
// none. The engine does not silently drop either declaration; it says so.
//
// Reddens on the mutation that keeps C270 silent on claw.
func TestC270FiresWhenClawWillRepopulateADeclaredEmptyToolList(t *testing.T) {
	r := compileSrc(t, emptyToolsBotWith("claw", "", "  interaction: human\n"))
	expectDiag(t, r, DiagEmptyToolsNotEnforced)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagEmptyToolsNotEnforced {
			msg = d.Message
		}
	}
	if !strings.Contains(msg, "ask_user") || !strings.Contains(msg, "interaction") {
		t.Errorf("the message must name the append and the declaration that causes it, got %q", msg)
	}
	// Without interaction, claw really does resolve zero tools: silent.
	expectNoDiag(t, compileSrc(t, emptyToolsBot("claw", "")), DiagEmptyToolsNotEnforced)
	// And a node that declared NAMES is not this diagnostic's subject.
	named := strings.Replace(emptyToolsBotWith("claw", "", "  interaction: human\n"), "tools: []", "tools: [read_file]", 1)
	expectNoDiag(t, compileSrc(t, named), DiagEmptyToolsNotEnforced)
}

// The studio's launch picker must disclose the crossing that actually loses
// the list. pi, kimi and grok never receive it, so a claw node retargeted
// there runs with that CLI's whole toolset — and the STRICTER the author was,
// the more they lose. Keyed on `len(tools) == 0` the picker said nothing for
// exactly the declared-empty case, while warning for `tools: [read_file]`,
// which loses strictly less; and the sentence it did emit ("the backend's
// full native toolset becomes available") is false on claude_code, which
// removes 13 of its 14 natives for that same list.
//
// Reddens on the mutation that keys the disclosure on the list's LENGTH
// instead of on whether the route receives it.
func TestTheLaunchPickerDisclosesTheRouteThatNeverReceivesTheToolList(t *testing.T) {
	for _, tc := range []struct {
		route string
		tools []string
		want  string
	}{
		{"pi", []string{}, "never passed"},
		{"kimi", []string{}, "never passed"},
		{"grok", []string{}, "never passed"},
		{"pi", []string{"read_file"}, "never passed"},
		{"claude_code", []string{}, ""},
		{"codex", []string{}, ""},
		{"claude_code", []string{"read_file"}, "narrowed rather than enforced"},
		{"claw", []string{}, ""},
		{"claude_code", nil, ""},
	} {
		got := ToolRestrictionLossReason("claw", tc.route, tc.tools)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("claw→%s with tools=%#v: unexpected warning %q", tc.route, tc.tools, got)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("claw→%s with tools=%#v: warning = %q, want it to say %q", tc.route, tc.tools, got, tc.want)
		}
	}
}
