package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/SocialGouv/iterion/pkg/store"
)

// rawControl reports the first rune of s, line by line, that a terminal would
// not show as printed text — judged by unicode.IsPrint, not by the escaping
// rule under test, so a rune both forget still reddens.
func rawControl(s string) (rune, bool) {
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		for _, r := range line {
			if !unicode.IsPrint(r) {
				return r, true
			}
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

// TestTerminalText_formatAndSeparatorCharactersAreInert: the characters that
// reorder or break what a terminal shows without being controls — line and
// paragraph separators, bidi marks, the Arabic letter mark, tag characters —
// are escaped too.
func TestTerminalText_formatAndSeparatorCharactersAreInert(t *testing.T) {
	for _, r := range []rune{0x2028, 0x2029, 0x200e, 0x200f, 0x061c, 0xe0041, 0x2066} {
		got := terminalText("a"+string(r)+"b", 100)
		if bad, raw := rawControl(got); raw {
			t.Fatalf("terminalText(%U) sends %U raw: %q", r, bad, got)
		}
	}
}

// TestPrinter_relayedValuesAreInert: a run's error and name reach the
// operator through KV and Table — a node's error carries what the run's
// processes wrote — so their values are shown inert; a blank event message
// does not hide the event's error.
func TestPrinter_relayedValuesAreInert(t *testing.T) {
	hostile := "boom\x1b]52;c;cGF5bG9hZA==\a\nHTTP 200 OK: resumed \u202eevil\u2028"
	out := &bytes.Buffer{}
	p := &Printer{W: out, Format: OutputHuman}
	p.KV("Error", hostile)
	p.Table([]string{"ID", "NAME"}, [][]string{{"r1", hostile}})
	if r, raw := rawControl(out.String()); raw {
		t.Fatalf("KV / Table send %U raw to the terminal: %q", r, out.String())
	}
	out.Reset()
	printRemoteEvent(p, remoteEvent{Type: "node_failed" + hostile, NodeID: "work" + hostile, Timestamp: time.Unix(0, 0)})
	if r, raw := rawControl(out.String()); raw {
		t.Fatalf("an event's type or node sends %U raw to the terminal: %q", r, out.String())
	}
	out.Reset()
	printRemoteEvent(p, remoteEvent{Type: "run_failed", Timestamp: time.Unix(0, 0), Data: map[string]any{"message": "  ", "error": "the real cause"}})
	if !strings.Contains(out.String(), "the real cause") {
		t.Fatalf("a blank message hid the event's error: %q", out.String())
	}
}

// TestRemoteRunsGet_aLongErrorKeepsItsRemedy: a run's error ends on its
// remedy — a scratch refusal names the consent that clears it — after a
// failure whose text embeds a process's output. `remote runs get` bounds the
// line on the failure's text: the remedy is shown whole, and inert.
func TestRemoteRunsGet_aLongErrorKeepsItsRemedy(t *testing.T) {
	var failure strings.Builder
	failure.WriteString("sandbox start: the sandbox's tar refuses the bank:")
	for i := range 60 {
		fmt.Fprintf(&failure, " tar: scratch/pack-%03d.idx: Cannot open: Permission denied\n", i)
	}
	runError := failure.String() + store.RunErrorHintSeparator + "relaunch the run fresh; or resume it accepting the scratch's loss (--accept-scratch-loss)\x1b[2J"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"id": "r1", "status": "failed_resumable", "error": runError}})
	}))
	defer srv.Close()
	var out bytes.Buffer
	c := &RemoteClient{cfg: RemoteConfig{BaseURL: srv.URL, Token: "tok"}, http: srv.Client()}
	if err := RemoteRunsGet(context.Background(), c, &Printer{W: &out}, "r1"); err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Error:") {
			line = l
		}
	}
	remedy := "…" + store.RunErrorHintSeparator + `relaunch the run fresh; or resume it accepting the scratch's loss (--accept-scratch-loss)\x1b[2J`
	if !strings.HasSuffix(line, remedy) || !strings.Contains(line, "pack-000") || strings.Contains(line, "pack-059") {
		t.Fatalf("`runs get` does not clip the failure and show the remedy whole: ...%q", line[max(0, len(line)-300):])
	}
	if r, raw := rawControl(line); raw {
		t.Fatalf("`runs get` sends %U raw to the terminal", r)
	}
}
