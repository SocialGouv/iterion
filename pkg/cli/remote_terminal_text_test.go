package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode"
)

// rawControl reports the first control or bidi-override rune s would send
// to a terminal as is.
func rawControl(s string) (rune, bool) {
	for _, r := range s {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return r, true
		}
	}
	return 0, false
}

// TestAPIError_sendsNoControlCharacterToTheTerminal: a server's error body
// relays text a run's processes wrote. Rendered for an operator, no field
// sends a control character (an escape sequence, a bell, a newline that
// forges a line, a bidi override) raw; every field is bounded; a body whose
// words are only blanks is shown by its first line.
func TestAPIError_sendsNoControlCharacterToTheTerminal(t *testing.T) {
	osc52 := "\x1b]52;c;cGF5bG9hZA==\a"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"error", `{"error":"refused` + "\\u001b[31m\\u0007\\nHTTP 200 OK: resumed" + `"}`, `\x1b`},
		{"hint", `{"error":"refused","hint":"` + "\\u001b]52;c;cGF5bG9hZA==\\u0007" + `"}`, `\x07`},
		{"error_code", "{\"error\":\"refused\",\"error_code\":\"x\u202eevil\"}", `\u202e`},
		{"reset_at", `{"error":"refused","retryable":true,"reset_at":"soon` + "\\r" + `now"}`, `\x0d`},
		{"not JSON", "plain\x1b[2Jtext\nsecond line", `\x1b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&APIError{Status: 409, Method: "POST", Path: "/api/runs/r/resume", Body: tc.body}).Error()
			if r, raw := rawControl(got); raw {
				t.Fatalf("the rendering sends %U raw to the terminal: %q", r, got)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("the rendering does not show the escaped %s: %q", tc.want, got)
			}
		})
	}
	long := strings.Repeat("c", 100000)
	if got := (&APIError{Status: 409, Body: `{"error":"refused","error_code":"` + long + `"}`}).Error(); len(got) > 3000 {
		t.Fatalf("an error_code of 100000 runes renders %d bytes, want it bounded", len(got))
	}
	if got := (&APIError{Status: 502, Method: "POST", Path: "/p", Body: `{"error":"   ","message":"\t"}`}).Error(); !strings.Contains(got, `"error"`) {
		t.Fatalf("a body whose words are blanks renders %q, want its first line", got)
	}
	out := &bytes.Buffer{}
	p := &Printer{W: out, Format: OutputHuman}
	printRemoteEvent(p, remoteEvent{Type: "run_failed", Timestamp: time.Unix(0, 0), Data: map[string]any{"error": "boom " + osc52, "hint": "retry" + osc52}})
	if r, raw := rawControl(out.String()); raw && r != '\n' {
		t.Fatalf("an event's error or hint sends %U raw to the terminal: %q", r, out.String())
	}
}
