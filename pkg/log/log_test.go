package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// TestLog_HumanUnchanged validates that the default format still emits
// "HH:MM:SS emoji message" lines — the byte-for-byte non-regression
// promise from the cloud-ready plan §K.
func TestLog_HumanUnchanged(t *testing.T) {
	var buf bytes.Buffer
	l := New(LevelInfo, &buf)
	l.Info("hello %s", "world")

	out := buf.String()
	if !strings.HasSuffix(out, "ℹ️  hello world\n") {
		t.Errorf("human output suffix mismatch: %q", out)
	}
	// Format prefix is "HH:MM:SS " (8 chars + space). Two digits each.
	if len(out) < 9 || out[2] != ':' || out[5] != ':' {
		t.Errorf("expected HH:MM:SS prefix, got %q", out)
	}
}

func TestLog_JSONShape(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelDebug, &buf, FormatJSON)
	l.Info("hi")

	var rec jsonRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not valid JSON: %v: %q", err, buf.String())
	}
	if rec.Msg != "hi" {
		t.Errorf("Msg: got %q want %q", rec.Msg, "hi")
	}
	if rec.Level != "info" {
		t.Errorf("Level: got %q want info", rec.Level)
	}
	if rec.TS == "" {
		t.Error("TS empty")
	}
	if rec.Fields != nil {
		t.Errorf("Fields: got %v want nil (root logger)", rec.Fields)
	}
}

func TestLog_WithFieldsCarriesContext(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	l2 := l.WithField("run_id", "run_42").WithField("node", "agent_1")
	l2.Info("started")

	var rec jsonRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if got := rec.Fields["run_id"]; got != "run_42" {
		t.Errorf("Fields[run_id]: got %v", got)
	}
	if got := rec.Fields["node"]; got != "agent_1" {
		t.Errorf("Fields[node]: got %v", got)
	}
}

func TestLog_WithFieldsForkIndependence(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	parent := l.WithField("trace", "abc")
	childA := parent.WithField("branch", "A")
	childB := parent.WithField("branch", "B")

	buf.Reset()
	parent.Info("p")
	childA.Info("a")
	childB.Info("b")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), buf.String())
	}
	var pr, ar, br jsonRecord
	if err := json.Unmarshal([]byte(lines[0]), &pr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &ar); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &br); err != nil {
		t.Fatal(err)
	}
	if _, ok := pr.Fields["branch"]; ok {
		t.Errorf("parent should not have 'branch' field: %v", pr.Fields)
	}
	if ar.Fields["branch"] != "A" {
		t.Errorf("childA branch = %v", ar.Fields["branch"])
	}
	if br.Fields["branch"] != "B" {
		t.Errorf("childB branch = %v", br.Fields["branch"])
	}
}

func TestLog_WithError(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	l.WithError(errors.New("boom")).Error("nope")

	var rec jsonRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Fields["error"] != "boom" {
		t.Errorf("error field: got %v", rec.Fields["error"])
	}
	if rec.Level != "error" {
		t.Errorf("Level: got %q", rec.Level)
	}
}

func TestLog_WithErrorNilNoOp(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	l.WithError(nil).Info("ok")

	var rec jsonRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Fields != nil {
		t.Errorf("expected no fields, got %v", rec.Fields)
	}
}

func TestLog_LevelGate(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelWarn, &buf, FormatJSON)
	l.Info("suppressed") // info > warn → not emitted
	l.Warn("kept")
	if !strings.Contains(buf.String(), `"msg":"kept"`) {
		t.Errorf("expected kept warn line, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "suppressed") {
		t.Errorf("info line leaked past warn level: %q", buf.String())
	}
}

func TestLog_JSONLogBlock(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	l.LogBlock(LevelInfo, "🔧", "header", "line1\nline2")

	var rec jsonRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Msg != "header" {
		t.Errorf("Msg: got %q", rec.Msg)
	}
	if rec.Fields["body"] != "line1\nline2" {
		t.Errorf("body: got %v", rec.Fields["body"])
	}
}

func TestLog_JSONConcurrentNoInterleave(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	const goroutines = 10
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			fl := l.WithField("g", g)
			for i := 0; i < perG; i++ {
				fl.Info("msg %d", i)
			}
		}(g)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != goroutines*perG {
		t.Fatalf("expected %d lines, got %d", goroutines*perG, len(lines))
	}
	for i, line := range lines {
		var rec jsonRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not JSON: %v: %q", i, err, line)
		}
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"":      LevelInfo,
		"info":  LevelInfo,
		"DEBUG": LevelDebug,
		"warn":  LevelWarn,
		"error": LevelError,
		"trace": LevelTrace,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseLevel("foo"); err == nil {
		t.Errorf("expected error on bad level")
	}
}

func TestNopLogger(t *testing.T) {
	l := Nop()
	l.Info("anything") // should not panic
	if l.IsEnabled(LevelError) {
		t.Errorf("Nop should not be enabled at any level")
	}
}

// TestTruncate exercises the byte-counted truncation: short strings pass
// through verbatim, over-length strings get the "...[truncated]" suffix,
// and a cut that lands mid-rune is pulled back to a rune boundary so the
// result stays valid UTF-8.
func TestTruncate(t *testing.T) {
	const suffix = "...[truncated]"
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"below max", "abc", 10, "abc"},
		{"exactly max", "abcd", 4, "abcd"},
		{"ascii over-length", "abcdefghij", 4, "abcd" + suffix},
		{"max zero", "abc", 0, suffix},
		{"max negative", "abc", -3, suffix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.in, tc.max); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}

	// Multibyte: cutting "héllo" at 2 bytes lands inside 'é' (0xC3 0xA9);
	// the result must remain valid UTF-8 (rune pulled back) and the kept
	// prefix must not exceed max bytes. A naive s[:max] would emit invalid
	// UTF-8 here.
	got := Truncate("héllo", 2)
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("Truncate(%q, 2) = %q, missing suffix", "héllo", got)
	}
	prefix := strings.TrimSuffix(got, suffix)
	if !utf8.ValidString(got) {
		t.Errorf("Truncate(%q, 2) = %q is not valid UTF-8", "héllo", got)
	}
	if len(prefix) > 2 {
		t.Errorf("Truncate(%q, 2) kept %d bytes %q, exceeds max", "héllo", len(prefix), prefix)
	}
	if prefix != "h" {
		t.Errorf("Truncate(%q, 2) kept %q, want %q (rune boundary)", "héllo", prefix, "h")
	}
}

// TestBlockPreview exercises the newline-preserving preview: empty input
// yields "", in-budget input passes through with newlines intact, and a
// truncated result appends the marker on its own line, rune-safe.
func TestBlockPreview(t *testing.T) {
	if got := BlockPreview("", 5); got != "" {
		t.Errorf("BlockPreview(\"\", 5) = %q, want empty", got)
	}
	// Non-empty input with max 0: nothing is kept, so the result is the
	// marker line alone (the empty-string short-circuit applies only to
	// empty input, not to a zero budget).
	if got := BlockPreview("anything", 0); got != "\n...[truncated]" {
		t.Errorf("BlockPreview(%q, 0) = %q, want marker only", "anything", got)
	}
	if got := BlockPreview("a\nb", 10); got != "a\nb" {
		t.Errorf("BlockPreview(%q, 10) = %q, want newlines preserved", "a\nb", got)
	}

	// Over-length multibyte: marker must land on its own line and the
	// kept portion must be valid UTF-8 with a rune-boundary cut.
	got := BlockPreview("héllo", 2)
	if !strings.HasSuffix(got, "\n...[truncated]") {
		t.Errorf("BlockPreview(%q, 2) = %q, want marker on its own line", "héllo", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("BlockPreview(%q, 2) = %q is not valid UTF-8", "héllo", got)
	}
	if kept := strings.TrimSuffix(got, "\n...[truncated]"); kept != "h" {
		t.Errorf("BlockPreview(%q, 2) kept %q, want %q (rune boundary)", "héllo", kept, "h")
	}
}

// TestSafeByteCut exercises the unexported rune-boundary walk-back directly.
// "héllo" is h(0x68) é(0xC3 0xA9) l l o — so byte index 2 is a continuation
// byte and index 3 is a clean rune boundary.
func TestSafeByteCut(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"max zero", "héllo", 0, ""},
		{"max negative", "héllo", -1, ""},
		{"max beyond length", "abc", 10, "abc"},
		{"cut on continuation byte pulls back", "héllo", 2, "h"},
		{"cut on rune boundary kept", "héllo", 3, "hé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeByteCut(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("safeByteCut(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("safeByteCut(%q, %d) = %q is not valid UTF-8", tc.in, tc.max, got)
			}
		})
	}
}

// TestResolveLevel verifies precedence: an explicit value wins over the
// env var, the env var is the fallback, an empty pair defaults to info,
// and a bad value on either path surfaces an error.
func TestResolveLevel(t *testing.T) {
	const envVar = "ITERION_TEST_LOG_LEVEL"
	// env "" means the var is effectively unset (ResolveLevel treats an
	// empty value as absent). On wantErr cases the returned level is the
	// LevelInfo fallback per the function contract.
	cases := []struct {
		name     string
		env      string
		explicit string
		wantErr  bool
		want     Level
	}{
		{"explicit wins over env", "error", "debug", false, LevelDebug},
		{"env fallback", "warn", "", false, LevelWarn},
		{"default when both empty", "", "", false, LevelInfo},
		{"explicit invalid errors", "info", "bogus", true, LevelInfo},
		{"env invalid errors", "bogus", "", true, LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envVar, tc.env)
			got, err := ResolveLevel(tc.explicit, envVar)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (level %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveLevel(%q, env=%q) = %v, want %v", tc.explicit, tc.env, got, tc.want)
			}
		})
	}
}

// TestLevelString covers every named level plus out-of-range values which
// must render as "unknown".
func TestLevelString(t *testing.T) {
	cases := []struct {
		level Level
		want  string
	}{
		{LevelError, "error"},
		{LevelWarn, "warn"},
		{LevelInfo, "info"},
		{LevelDebug, "debug"},
		{LevelTrace, "trace"},
		{Level(99), "unknown"},
		{Level(-1), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.level.String(); got != tc.want {
			t.Errorf("Level(%d).String() = %q, want %q", int(tc.level), got, tc.want)
		}
	}
}

// TestLogBlock_HumanFormat covers the human-readable LogBlock path: the
// header line, indented continuation lines, the trailing-newline guard,
// and level gating.
func TestLogBlock_HumanFormat(t *testing.T) {
	t.Run("header only", func(t *testing.T) {
		var buf bytes.Buffer
		l := New(LevelInfo, &buf)
		l.LogBlock(LevelInfo, "🔧", "header", "")
		out := buf.String()
		if !strings.HasSuffix(out, " 🔧 header\n") {
			t.Errorf("header line mismatch: %q", out)
		}
		if strings.Contains(out, blockIndent) {
			t.Errorf("empty body must not emit indented lines: %q", out)
		}
		if n := strings.Count(out, "\n"); n != 1 {
			t.Errorf("header-only should be one line, got %d: %q", n, out)
		}
	})

	t.Run("multi-line body indented", func(t *testing.T) {
		var buf bytes.Buffer
		l := New(LevelInfo, &buf)
		l.LogBlock(LevelInfo, "🔧", "head", "line1\nline2")
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("expected header + 2 body lines, got %d: %q", len(lines), buf.String())
		}
		if lines[1] != blockIndent+"line1" {
			t.Errorf("body line 1 = %q, want %q", lines[1], blockIndent+"line1")
		}
		if lines[2] != blockIndent+"line2" {
			t.Errorf("body line 2 = %q, want %q", lines[2], blockIndent+"line2")
		}
	})

	t.Run("trailing newline does not emit blank line", func(t *testing.T) {
		var buf bytes.Buffer
		l := New(LevelInfo, &buf)
		l.LogBlock(LevelInfo, "🔧", "head", "a\nb\n")
		// Header + exactly two indented lines; no stray blank indented line.
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		if len(lines) != 3 {
			t.Fatalf("trailing newline must not add a blank line; got %d lines: %q", len(lines), buf.String())
		}
		if strings.Contains(buf.String(), blockIndent+"\n") {
			t.Errorf("found stray blank indented line: %q", buf.String())
		}
	})

	t.Run("level gating suppresses output", func(t *testing.T) {
		var buf bytes.Buffer
		l := New(LevelInfo, &buf)
		l.LogBlock(LevelDebug, "🔧", "head", "body")
		if buf.Len() != 0 {
			t.Errorf("debug block on info logger should emit nothing, got %q", buf.String())
		}
	})
}

// TestEmojiPrefixes verifies each leveled helper writes its documented
// emoji and message in human format, that Logf honours a custom emoji, and
// that a below-threshold helper is gated out.
func TestEmojiPrefixes(t *testing.T) {
	cases := []struct {
		name  string
		emoji string
		call  func(l *Logger)
	}{
		{"error", "❌", func(l *Logger) { l.Error("boom %d", 1) }},
		{"warn", "⚠️", func(l *Logger) { l.Warn("careful") }},
		{"debug", "🔍", func(l *Logger) { l.Debug("trace it") }},
		{"trace", "🔬", func(l *Logger) { l.Trace("deep") }},
		{"logf custom", "🚀", func(l *Logger) { l.Logf(LevelInfo, "🚀", "launch") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			// Trace is the most verbose level; enables every helper.
			l := New(LevelTrace, &buf)
			tc.call(l)
			if !strings.Contains(buf.String(), tc.emoji) {
				t.Errorf("%s output missing emoji %q: %q", tc.name, tc.emoji, buf.String())
			}
		})
	}

	// Logf message content is carried through.
	var buf bytes.Buffer
	l := New(LevelTrace, &buf)
	l.Logf(LevelInfo, "🚀", "value=%d", 42)
	if !strings.Contains(buf.String(), "value=42") {
		t.Errorf("Logf dropped formatted message: %q", buf.String())
	}

	// Gating: Debug on an Info logger emits nothing.
	buf.Reset()
	gated := New(LevelInfo, &buf)
	gated.Debug("should be suppressed")
	if buf.Len() != 0 {
		t.Errorf("debug on info logger leaked output: %q", buf.String())
	}
}

// TestNilReceiverSafety pins the documented nil-*Logger contracts — the
// optional-logger idiom relies on every guarded method being safe.
func TestNilReceiverSafety(t *testing.T) {
	var l *Logger

	if got := l.Level(); got != LevelInfo {
		t.Errorf("nil.Level() = %v, want LevelInfo", got)
	}
	if got := l.Writer(); got != io.Discard {
		t.Errorf("nil.Writer() = %v, want io.Discard", got)
	}
	if l.IsEnabled(LevelError) {
		t.Error("nil.IsEnabled(LevelError) = true, want false")
	}
	if got := l.WithField("k", "v"); got != nil {
		t.Errorf("nil.WithField() = %v, want nil", got)
	}
	if got := l.WithFields(map[string]any{"k": "v"}); got != nil {
		t.Errorf("nil.WithFields() = %v, want nil", got)
	}
	if got := l.WithError(errors.New("x")); got != nil {
		t.Errorf("nil.WithError() = %v, want nil", got)
	}
	// Must not panic.
	l.Info("ignored")
	l.LogBlock(LevelInfo, "🔧", "h", "b")
}

// TestNewWithFormatNilWriter verifies a nil writer is swapped for
// io.Discard so the silence-all-output idiom never panics.
func TestNewWithFormatNilWriter(t *testing.T) {
	l := New(LevelInfo, nil)
	l.Info("no panic") // would nil-pointer-panic without the io.Discard swap
	if got := l.Writer(); got != io.Discard {
		t.Errorf("Writer() = %v, want io.Discard for nil writer", got)
	}

	l2 := NewWithFormat(LevelInfo, nil, FormatJSON)
	l2.Info("also fine")
	if got := l2.Writer(); got != io.Discard {
		t.Errorf("NewWithFormat Writer() = %v, want io.Discard", got)
	}
}

// TestWithFieldsEmptyAndMulti verifies an empty/nil field map produces a
// usable, field-less fork and that multiple keys are all carried.
func TestWithFieldsEmptyAndMulti(t *testing.T) {
	t.Run("nil and empty maps add no fields", func(t *testing.T) {
		for _, fields := range []map[string]any{nil, {}} {
			var buf bytes.Buffer
			l := NewWithFormat(LevelInfo, &buf, FormatJSON)
			l.WithFields(fields).Info("hi")
			var rec jsonRecord
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("not valid JSON: %v", err)
			}
			if rec.Fields != nil {
				t.Errorf("expected no fields, got %v", rec.Fields)
			}
			if rec.Msg != "hi" {
				t.Errorf("Msg = %q, want hi", rec.Msg)
			}
		}
	})

	t.Run("multiple keys carried", func(t *testing.T) {
		var buf bytes.Buffer
		l := NewWithFormat(LevelInfo, &buf, FormatJSON)
		l.WithFields(map[string]any{"a": "1", "b": "2"}).Info("hi")
		var rec jsonRecord
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		if rec.Fields["a"] != "1" || rec.Fields["b"] != "2" {
			t.Errorf("WithFields lost keys: %v", rec.Fields)
		}
	})
}

// TestWriteJSON_Timestamp_And_MarshalFailure pins the UTC RFC3339Nano
// timestamp contract and the "a marshal failure never crashes the
// producer, it drops the line" guarantee.
func TestWriteJSON_Timestamp(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	l.Info("tick")

	var rec jsonRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, rec.TS); err != nil {
		t.Errorf("ts %q not RFC3339Nano: %v", rec.TS, err)
	}
	if !strings.HasSuffix(rec.TS, "Z") {
		t.Errorf("ts %q is not UTC (expected trailing Z)", rec.TS)
	}
}

func TestWriteJSON_MarshalFailureDropsLine(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON)
	// A channel value cannot be JSON-marshalled; the record is dropped
	// silently rather than panicking or writing a partial line.
	l.WithField("bad", make(chan int)).Info("nope")
	if buf.Len() != 0 {
		t.Errorf("expected no output on marshal failure, got %q", buf.String())
	}
}

func TestLog_WithWriterKeepsFormatFieldsAndHook(t *testing.T) {
	var parent, tee bytes.Buffer
	root := NewWithFormat(LevelInfo, &parent, FormatJSON).WithField("run_id", "run_42")

	var hooked []Level
	root.SetHook(func(level Level, _ string, fields map[string]any) {
		if fields["run_id"] != "run_42" {
			t.Errorf("hook lost the inherited fields: %v", fields)
		}
		hooked = append(hooked, level)
	})

	// A per-run logger tees into its own buffer AND the parent's writer,
	// exactly as the runner and the run console build theirs.
	run := root.WithWriter(io.MultiWriter(root.Writer(), &tee))
	run.Error("boom")

	var rec jsonRecord
	if err := json.Unmarshal(tee.Bytes(), &rec); err != nil {
		t.Fatalf("the fork did not keep the JSON format: %v: %q", err, tee.String())
	}
	if rec.Msg != "boom" || rec.Level != "error" {
		t.Errorf("record: got level=%q msg=%q", rec.Level, rec.Msg)
	}
	if rec.Fields["run_id"] != "run_42" {
		t.Errorf("the fork lost the inherited fields: %v", rec.Fields)
	}
	if parent.String() != tee.String() {
		t.Errorf("the parent's writer did not see the line: parent=%q tee=%q", parent.String(), tee.String())
	}
	if len(hooked) != 1 || hooked[0] != LevelError {
		t.Errorf("the fork did not reach the root's hook: %v", hooked)
	}
}

func TestLog_WithWriterNilSafe(t *testing.T) {
	var nilLogger *Logger
	if got := nilLogger.WithWriter(io.Discard); got != nil {
		t.Errorf("WithWriter on a nil logger: got %v want nil", got)
	}
	// A nil writer degrades to Discard rather than panicking on write.
	NewWithFormat(LevelInfo, nil, FormatHuman).WithWriter(nil).Info("no panic")
}

// What LogBlock writes for a body is the body plus a continuation prefix per
// LINE, so a bound on the body alone is not a bound on the log. The property
// below is read off the WRITER, for the shapes a third party chooses between:
// one byte per line (the worst case for the prefix), long lines (where the
// prefix is noise), no newline at all, and nothing but newlines (which the
// renderer trims to a single empty line).
func TestBlockPreviewRenderedBoundsWhatTheWriterReceives(t *testing.T) {
	bodies := map[string]string{
		"one byte per line":    strings.Repeat("d\n", 300000),
		"thousand-byte lines":  strings.Repeat(strings.Repeat("d", 1000)+"\n", 600),
		"no newline at all":    strings.Repeat("d", 400000),
		"nothing but newlines": strings.Repeat("\n", 200000),
	}
	// Including the budgets too small for a prefixed line, where a marker
	// nobody asked for would itself break the bound.
	for _, max := range []int{4, 27, 28, 42, 43, 1 << 12, 1 << 16, 1 << 20} {
		for name, body := range bodies {
			t.Run(fmt.Sprintf("%s/max=%d", name, max), func(t *testing.T) {
				kept := BlockPreviewRendered(body, max)
				var buf bytes.Buffer
				New(LevelInfo, &buf).LogBlock(LevelInfo, "📄", "header:", kept)
				// The header line is the caller's, not the body's budget.
				written := buf.Len() - len("15:04:05 📄 header:\n")
				if written > max {
					t.Errorf("the writer received %d bytes of body for a %d-byte bound", written, max)
				}
				if renderedBlockSize(body) <= max {
					// Under the bound: handed on as produced, no marker.
					if kept != body {
						t.Errorf("a body rendering in %d bytes was cut under a %d-byte bound", renderedBlockSize(body), max)
					}
					return
				}
				// A budget below one prefixed line plus the marker cannot
				// render a body at all: the header goes out alone.
				if max < len(blockIndent)+1+len("...[truncated]") {
					if kept != "" {
						t.Errorf("a %d-byte bound kept %q, want no body: the marker alone renders past it", max, kept)
					}
					return
				}
				// Over the bound: cut, marked, and — once the budget is big
				// enough to carry a diagnostic — most of the budget used. A
				// bound that keeps nothing is unusable.
				if !strings.Contains(kept, "...[truncated]") {
					t.Errorf("an over-bound body was cut without the marker: %.40q", kept)
				}
				if max >= 1<<12 && written < max/2 {
					t.Errorf("the writer received only %d bytes for a %d-byte bound — the cut throws away the diagnostic", written, max)
				}
			})
		}
	}
	// The marker alone, for a budget that fits it and nothing else. (A body
	// whose own rendering fits is returned whole, however small max is — the
	// bound is a maximum, not a target.)
	long := strings.Repeat("d", 64)
	if got := BlockPreviewRendered(long, 28); got != "...[truncated]" {
		t.Errorf("BlockPreviewRendered(64 bytes, 28) = %q, want the marker alone", got)
	}
	if got := BlockPreviewRendered("tiny", 28); got != "tiny" {
		t.Errorf("BlockPreviewRendered(%q, 28) = %q, want it whole: its own rendering fits", "tiny", got)
	}
	if got := BlockPreviewRendered("", 1<<10); got != "" {
		t.Errorf("BlockPreviewRendered(\"\") = %q, want empty", got)
	}
	// The cut stays on a rune boundary, like Truncate's — at every budget
	// where a cut actually HAPPENS. A budget below one prefixed line plus the
	// marker returns no body at all, and "" is valid UTF-8 whatever the cut
	// does, so that budget gets the assertion it can carry: no body.
	multiByte := strings.Repeat("é", 100)
	if got := BlockPreviewRendered(multiByte, 20); got != "" {
		t.Errorf("BlockPreviewRendered(200 bytes, 20) = %q, want no body: one prefixed line plus the marker is already past 20", got)
	}
	// Two bytes per rune and one line, so this test knows the rendering
	// without asking the subject: the body plus one line's prefix and
	// newline. The band below is every budget at which a byte cut actually
	// HAPPENS — from the smallest that keeps a body (the marker's own
	// prefixed line, plus one for the body) to the largest that still cuts
	// (one under the whole body's rendering). Below it the function returns
	// the marker alone and above it the body whole, and utf8.ValidString has
	// nothing to watch there, so each budget also has to produce a MARKED cut
	// of whole runes: a band that drifts off the cutting range reddens here
	// instead of going vacuous. The smallest that keeps a body is the marker's
	// own prefixed line, the body's first prefixed line, and one byte of body.
	const perLine = len(blockIndent) + 1
	const smallestBodyBudget = 2*perLine + len("...[truncated]") + 1
	if got := BlockPreviewRendered(multiByte, smallestBodyBudget-1); got != "...[truncated]" {
		t.Errorf("one byte under the band: BlockPreviewRendered = %q, want the marker alone", got)
	}
	for max := smallestBodyBudget; max < len(multiByte)+len(blockIndent)+1; max++ {
		got := BlockPreviewRendered(multiByte, max)
		if !utf8.ValidString(got) {
			t.Errorf("max=%d: BlockPreviewRendered cut inside a rune: %q", max, got)
		}
		body, marked := strings.CutSuffix(got, "\n...[truncated]")
		if !marked || strings.Trim(body, "é") != "" {
			t.Errorf("max=%d: want a marked cut of whole runes, got %q", max, got)
		}
	}
}

// The bound AT the boundary, read off the writer against an accounting this
// test does itself.
//
// The property above picks its assertion by calling renderedBlockSize — the
// subject's own oracle — and every body it drives is far over or far under
// every budget, so an accounting that misjudges a body by one line's prefix
// agrees with itself and ships. This shape is one the test can account for:
// 1000 single-byte lines, each written as the continuation prefix, its byte,
// and the newline the renderer puts back. The budgets sweep the whole band
// where an accounting short by one newline per line and the real renderer
// disagree.
func TestBlockPreviewRenderedHoldsAtTheBoundary(t *testing.T) {
	const lines = 1000
	const perLine = len(blockIndent) + 2 // prefix + one byte + newline
	const rendered = lines * perLine
	body := strings.Repeat("d\n", lines)
	for max := lines*len(blockIndent) - 100; max <= rendered+1; max++ {
		kept := BlockPreviewRendered(body, max)
		var buf bytes.Buffer
		New(LevelInfo, &buf).LogBlock(LevelInfo, "📄", "header:", kept)
		written := buf.Len() - len("15:04:05 📄 header:\n")
		if written > max {
			t.Fatalf("max=%d: the writer received %d bytes of body", max, written)
		}
		if max >= rendered && kept != body {
			t.Fatalf("max=%d: a body rendering in %d bytes came back cut", max, rendered)
		}
	}
}

// The same bound over shapes nobody enumerated. The accounting has one term
// per line and one per byte, so what breaks it is a SHAPE — where the
// newlines fall, how long the runs are, how many bytes a rune takes — and the
// third party choosing the text chooses the shape. Read off the real writer,
// with the cut's rune safety on the same pass.
func TestBlockPreviewRenderedBoundsRandomShapes(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 4000; i++ {
		var sb strings.Builder
		for j := 1 + r.Intn(400); j > 0; j-- {
			switch r.Intn(4) {
			case 0:
				sb.WriteByte('\n')
			case 1:
				sb.WriteString(strings.Repeat("d", 1+r.Intn(40)))
			case 2:
				sb.WriteString("é")
			default:
				sb.WriteString("dd\n")
			}
		}
		body, max := sb.String(), 1+r.Intn(900)
		kept := BlockPreviewRendered(body, max)
		var buf bytes.Buffer
		New(LevelInfo, &buf).LogBlock(LevelInfo, "📄", "header:", kept)
		written := buf.Len() - len("15:04:05 📄 header:\n")
		if written > max {
			t.Fatalf("i=%d max=%d: the writer received %d bytes of body for %q", i, max, written, body)
		}
		if !utf8.ValidString(kept) {
			t.Fatalf("i=%d max=%d: the cut left invalid UTF-8: %q (body %q)", i, max, kept, body)
		}
	}
}
