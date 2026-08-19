package log

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// record is one hook invocation captured by recorder.
type record struct {
	level  Level
	msg    string
	fields map[string]any
}

type recorder struct {
	mu   sync.Mutex
	recs []record
}

func (r *recorder) hook(level Level, msg string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, record{level: level, msg: msg, fields: fields})
}

func (r *recorder) all() []record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]record, len(r.recs))
	copy(out, r.recs)
	return out
}

func TestHookFiresAtWarnAndAbove(t *testing.T) {
	var buf bytes.Buffer
	rec := &recorder{}
	l := New(LevelTrace, &buf)
	l.SetHook(rec.hook)

	l.Error("boom %d", 1)
	l.Warn("careful")
	l.Info("fyi")
	l.Debug("noisy")
	l.Trace("noisier")

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("want 2 hook calls (error+warn), got %d: %+v", len(got), got)
	}
	if got[0].level != LevelError || got[0].msg != "boom 1" {
		t.Errorf("first call = %v %q, want error \"boom 1\"", got[0].level, got[0].msg)
	}
	if got[1].level != LevelWarn || got[1].msg != "careful" {
		t.Errorf("second call = %v %q, want warn \"careful\"", got[1].level, got[1].msg)
	}
}

func TestHookNotFiredBelowLoggerLevel(t *testing.T) {
	rec := &recorder{}
	l := New(LevelError, nil)
	l.SetHook(rec.hook)

	l.Warn("suppressed")
	l.Error("kept")

	got := rec.all()
	if len(got) != 1 || got[0].level != LevelError {
		t.Fatalf("want only the error record, got %+v", got)
	}
}

func TestHookReceivesFieldsAndForksShareTheSlot(t *testing.T) {
	rec := &recorder{}
	root := New(LevelInfo, nil)
	// Fork BEFORE installing the hook: the slot is shared, so the fork
	// must still see it.
	child := root.WithField("run_id", "r-1").WithFields(map[string]any{"node": "implement"})
	root.SetHook(rec.hook)

	child.Error("node failed")

	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("want 1 hook call, got %d", len(got))
	}
	if got[0].fields["run_id"] != "r-1" || got[0].fields["node"] != "implement" {
		t.Fatalf("fields = %+v, want run_id=r-1 node=implement", got[0].fields)
	}

	// The snapshot is a copy: mutating it must not corrupt the logger.
	got[0].fields["run_id"] = "tampered"
	rec2 := &recorder{}
	child.SetHook(rec2.hook)
	child.Error("again")
	if v := rec2.all()[0].fields["run_id"]; v != "r-1" {
		t.Fatalf("run_id = %v after hook mutated its snapshot, want r-1", v)
	}
}

func TestHookPanicDoesNotPropagate(t *testing.T) {
	var buf bytes.Buffer
	l := New(LevelInfo, &buf)
	l.SetHook(func(Level, string, map[string]any) {
		panic("hook exploded")
	})

	// Must not panic, and the original line must still be written.
	l.Error("the real message")

	out := buf.String()
	if !strings.Contains(out, "the real message") {
		t.Errorf("original line missing from output: %q", out)
	}
	if !strings.Contains(out, "hook panicked") {
		t.Errorf("hook panic not reported: %q", out)
	}
}

func TestSetHookNilClears(t *testing.T) {
	rec := &recorder{}
	l := New(LevelInfo, nil)
	l.SetHook(rec.hook)
	l.Error("first")
	l.SetHook(nil)
	l.Error("second")

	if got := rec.all(); len(got) != 1 || got[0].msg != "first" {
		t.Fatalf("want only \"first\", got %+v", got)
	}
}

func TestNoHookIsANoOp(t *testing.T) {
	var buf bytes.Buffer
	l := New(LevelInfo, &buf)
	l.Error("plain")
	if !strings.Contains(buf.String(), "plain") {
		t.Fatalf("output = %q", buf.String())
	}
	// Nil logger + nil-slot logger stay safe.
	var nilLogger *Logger
	nilLogger.SetHook(rec2Hook)
	nilLogger.Error("ignored")
	(&Logger{level: LevelInfo, w: &buf, mu: &sync.Mutex{}}).Error("hookless")
}

func rec2Hook(Level, string, map[string]any) {}

func TestLogBlockDispatchesHookWithBody(t *testing.T) {
	for _, format := range []Format{FormatHuman, FormatJSON} {
		rec := &recorder{}
		l := NewWithFormat(LevelInfo, nil, format)
		l.SetHook(rec.hook)

		l.LogBlock(LevelError, "❌", "verify failed", "exit status 1\nFAIL ./pkg/x")

		got := rec.all()
		if len(got) != 1 {
			t.Fatalf("format %v: want 1 hook call, got %d", format, len(got))
		}
		if got[0].msg != "verify failed" {
			t.Errorf("format %v: msg = %q", format, got[0].msg)
		}
		if body, _ := got[0].fields["body"].(string); !strings.Contains(body, "FAIL ./pkg/x") {
			t.Errorf("format %v: body field = %v", format, got[0].fields["body"])
		}
	}
}

func TestNopLoggerNeverDispatches(t *testing.T) {
	rec := &recorder{}
	l := Nop()
	l.SetHook(rec.hook)
	l.Error("swallowed")
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("Nop logger dispatched %+v", got)
	}
}
