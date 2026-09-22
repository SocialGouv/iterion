package server

import "testing"

// A dialled node pin is a backend: `${VAR:-claw}` IS claw, so the picker's
// capability-drift disclosure must reach it. Reading the layer raw made the
// warning disappear on exactly the nodes that carry a dial.
func TestEffectivePreviewBackend_ReadsADialByItsDefault(t *testing.T) {
	cases := []struct{ node, run, workflow, env, want string }{
		{"${PREVIEW_UNSET:-claw}", "", "", "", "claw"},
		{"", "${PREVIEW_UNSET:-claude_code}", "", "", "claude_code"},
		{"", "", "${PREVIEW_UNSET:-kimi}", "", "kimi"},
		{"", "", "", "${PREVIEW_UNSET:-grok}", "grok"},
		// `auto` and an unanswerable reference are not names: the next
		// layer decides, and the last one leaves it unknown.
		{"${PREVIEW_UNSET:-auto}", "claw", "", "", "claw"},
		{"${PREVIEW_UNSET}", "claw", "", "", "claw"},
		{"{{vars.b}}", "", "", "", ""},
		{"claw", "claude_code", "", "", "claw"},
		{"", "", "", "", ""},
	}
	for _, c := range cases {
		if got := effectivePreviewBackend(c.node, c.run, c.workflow, c.env); got != c.want {
			t.Errorf("effectivePreviewBackend(%q, %q, %q, %q) = %q, want %q",
				c.node, c.run, c.workflow, c.env, got, c.want)
		}
	}
}
