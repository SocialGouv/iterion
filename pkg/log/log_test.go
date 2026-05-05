package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
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
