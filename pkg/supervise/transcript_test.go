package supervise

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// runHandleLine feeds lines through handleLine with a collecting emit,
// returning the synthesized events. seen/toolNames persist across lines
// like the tailer's.
func runHandleLine(t *testing.T, lines []string) []*store.Event {
	t.Helper()
	o := &TranscriptObserver{}
	seen := map[string]string{}
	toolNames := map[string]string{}
	var got []*store.Event
	emit := func(typ store.EventType, ts time.Time, data map[string]any) bool {
		got = append(got, &store.Event{Type: typ, Timestamp: ts, Data: data})
		return true
	}
	for _, l := range lines {
		if cont := o.handleLine([]byte(l), seen, toolNames, emit); !cont {
			t.Fatalf("handleLine returned false for %q", l)
		}
	}
	return got
}

func TestHandleLineSynthesis(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  []store.EventType
	}{
		{"empty and whitespace lines tolerated", []string{"", "   "}, nil},
		{"torn/foreign line tolerated", []string{`{"type":"assistant","mess`}, nil},
		{"non-JSON garbage tolerated", []string{"not json at all"}, nil},
		{"meta record skipped",
			[]string{`{"type":"assistant","isMeta":true,"message":{"content":[{"type":"text","text":"hi"}]}}`}, nil},
		{"sidechain record skipped",
			[]string{`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"hi"}]}}`}, nil},
		{"compact summary skipped",
			[]string{`{"type":"assistant","isCompactSummary":true,"message":{"content":[{"type":"text","text":"hi"}]}}`}, nil},
		{"uuid dedup drops the replayed record",
			[]string{
				`{"type":"assistant","uuid":"u1","message":{"content":[{"type":"text","text":"done"}]}}`,
				`{"type":"assistant","uuid":"u1","message":{"content":[{"type":"text","text":"done"}]}}`,
			},
			[]store.EventType{store.EventLLMStepFinished}},
		{"records without uuid are never deduped",
			[]string{
				`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}]}}`,
				`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}]}}`,
			},
			[]store.EventType{store.EventLLMStepFinished, store.EventLLMStepFinished}},
		{"assistant text yields a turn boundary",
			[]string{`{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":"All done."}]}}`},
			[]store.EventType{store.EventLLMStepFinished}},
		{"assistant tool_use plus text is NOT a turn boundary",
			[]string{`{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":"running"},{"type":"tool_use","id":"t1","name":"Bash"}]}}`},
			[]store.EventType{store.EventToolCalled}},
		{"assistant empty text does not yield a boundary",
			[]string{`{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":""}]}}`},
			nil},
		{"user non-error tool_result ignored",
			[]string{`{"type":"user","uuid":"u","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false}]}}`},
			nil},
		{"user plain string content ignored",
			[]string{`{"type":"user","uuid":"u","message":{"content":"please fix the tests"}}`},
			nil},
		{"unknown record type ignored",
			[]string{`{"type":"summary","uuid":"s"}`},
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runHandleLine(t, tc.lines)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events (%+v); want %d", len(got), got, len(tc.want))
			}
			for i, typ := range tc.want {
				if got[i].Type != typ {
					t.Errorf("event %d = %s; want %s", i, got[i].Type, typ)
				}
			}
		})
	}
}

func TestHandleLineToolErrorNameResolution(t *testing.T) {
	t.Run("known tool_use_id tags the error with the tool name", func(t *testing.T) {
		got := runHandleLine(t, []string{
			`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`,
			`{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true}]}}`,
		})
		if len(got) != 2 {
			t.Fatalf("got %d events; want 2", len(got))
		}
		if got[0].Type != store.EventToolCalled || eventToolName(got[0]) != "Bash" {
			t.Errorf("event0 = %s/%s; want tool_called Bash", got[0].Type, eventToolName(got[0]))
		}
		if got[1].Type != store.EventToolError || eventToolName(got[1]) != "Bash" {
			t.Errorf("event1 = %s/%s; want tool_error Bash", got[1].Type, eventToolName(got[1]))
		}
	})

	t.Run("unknown tool_use_id yields an untagged tool_error", func(t *testing.T) {
		got := runHandleLine(t, []string{
			`{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"never-seen","is_error":true}]}}`,
		})
		if len(got) != 1 || got[0].Type != store.EventToolError {
			t.Fatalf("got %+v; want one tool_error", got)
		}
		if name := eventToolName(got[0]); name != "" {
			t.Errorf("tool name = %q; want empty for unknown tool_use_id", name)
		}
		if got[0].Data["error"] != "tool_result reported an error" {
			t.Errorf("error data = %v", got[0].Data["error"])
		}
	})
}

func TestHandleLineStopsWhenEmitDeclines(t *testing.T) {
	o := &TranscriptObserver{}
	emit := func(store.EventType, time.Time, map[string]any) bool { return false }
	line := []byte(`{"type":"assistant","uuid":"a","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`)
	if cont := o.handleLine(line, map[string]string{}, map[string]string{}, emit); cont {
		t.Fatal("handleLine must return false when emit declines (ctx done)")
	}
}

func TestDecodeContent(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    int
	}{
		{"empty message", "", 0},
		{"invalid JSON", "{oops", 0},
		{"no content key", `{"role":"assistant"}`, 0},
		{"array form", `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Bash"}]}`, 2},
		{"bare string form", `{"content":"a plain prompt"}`, 1},
		{"empty string content", `{"content":""}`, 0},
		{"non-array non-string content", `{"content":42}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeContent([]byte(tc.message))
			if len(got) != tc.want {
				t.Fatalf("decodeContent = %+v; want %d blocks", got, tc.want)
			}
		})
	}

	t.Run("bare string becomes a text block", func(t *testing.T) {
		got := decodeContent([]byte(`{"content":"hello"}`))
		if len(got) != 1 || got[0].Type != "text" || got[0].Text != "hello" {
			t.Fatalf("decodeContent = %+v; want one text block", got)
		}
	})
}

func TestParseTS(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantZero bool
	}{
		{"empty", "", true},
		{"garbage", "yesterday", true},
		{"RFC3339", "2026-06-25T10:00:00Z", false},
		{"RFC3339Nano", "2026-06-25T10:00:00.123456789Z", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTS(tc.in)
			if got.IsZero() != tc.wantZero {
				t.Errorf("parseTS(%q).IsZero() = %v; want %v", tc.in, got.IsZero(), tc.wantZero)
			}
		})
	}
	if got := parseTS("2026-06-25T10:00:00Z"); !got.Equal(time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("parseTS RFC3339 = %v", got)
	}
}

func TestReadFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")

	t.Run("missing file is not an error", func(t *testing.T) {
		data, off, err := readFrom(filepath.Join(dir, "nope.jsonl"), 7)
		if err != nil || data != nil || off != 7 {
			t.Fatalf("readFrom missing = (%v, %d, %v); want (nil, 7, nil)", data, off, err)
		}
	})

	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, off, err := readFrom(path, 0)
	if err != nil || string(data) != "hello\n" || off != 6 {
		t.Fatalf("initial read = (%q, %d, %v)", data, off, err)
	}

	// No new bytes → empty.
	data, off2, err := readFrom(path, off)
	if err != nil || len(data) != 0 || off2 != off {
		t.Fatalf("no-growth read = (%q, %d, %v)", data, off2, err)
	}

	// Appended bytes are read from the offset.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("world\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, off3, err := readFrom(path, off)
	if err != nil || string(data) != "world\n" || off3 != 12 {
		t.Fatalf("append read = (%q, %d, %v)", data, off3, err)
	}

	// A shrunk file (session rotation) restarts from 0.
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, off4, err := readFrom(path, off3)
	if err != nil || string(data) != "new\n" || off4 != 4 {
		t.Fatalf("shrunk read = (%q, %d, %v); want restart from 0", data, off4, err)
	}
}
