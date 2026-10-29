package log

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// testRedactor is a stand-in for the run's secret guard: it swaps one known
// value for its placeholder, and counts calls so the level-gate test can
// prove a dropped record is never scanned.
func testRedactor(calls *int) func(string) string {
	return func(s string) string {
		*calls++
		return strings.ReplaceAll(s, "sk-live-9f3Kd8mQ2wXp", "__ITERION_SECRET_test__")
	}
}

// The chokepoint of #2458: one redactor on the run's logger scrubs every
// kept record — line, block header and body, JSON fields, and what the
// warn+ hook (error tracking) receives — so a producer hands third-party
// text to the logger raw and no per-sink pass is owed.
func TestWithRedactorScrubsEverySink(t *testing.T) {
	var buf bytes.Buffer
	var hookedMsg, hookedBody, hookedField string
	l := NewWithFormat(LevelDebug, &buf, FormatJSON).WithRedactor(testRedactor(new(int)))
	l = l.WithField("route", "key sk-live-9f3Kd8mQ2wXp here")
	l.SetHook(func(level Level, msg string, fields map[string]any) {
		hookedMsg = msg
		hookedBody, _ = fields["body"].(string)
		hookedField, _ = fields["route"].(string)
	})

	l.Info("call failed with sk-live-9f3Kd8mQ2wXp")
	l.LogBlock(LevelInfo, "🧠", "header sk-live-9f3Kd8mQ2wXp", "body sk-live-9f3Kd8mQ2wXp")
	l.Warn("warn with sk-live-9f3Kd8mQ2wXp")

	out := buf.String()
	if strings.Contains(out, "sk-live-9f3Kd8mQ2wXp") {
		t.Errorf("the written stream still carries the raw value: %q", out)
	}
	if !strings.Contains(out, "__ITERION_SECRET_test__") {
		t.Errorf("the written stream shows no placeholder: %q", out)
	}
	// The hook (error tracking) receives the same redacted text, including
	// the block body and the inherited field it rides with.
	if strings.Contains(hookedMsg, "sk-live-9f3Kd8mQ2wXp") ||
		strings.Contains(hookedBody, "sk-live-9f3Kd8mQ2wXp") ||
		strings.Contains(hookedField, "sk-live-9f3Kd8mQ2wXp") {
		t.Errorf("the hook received raw text: msg=%q body=%q field=%q", hookedMsg, hookedBody, hookedField)
	}
	if !strings.Contains(hookedMsg, "__ITERION_SECRET_test__") {
		t.Errorf("the hook received no placeholder: %q", hookedMsg)
	}
}

// The scan runs after the level test: a record the logger drops must never
// be redacted — a quiet run pays nothing for the chokepoint.
func TestWithRedactorSkippedBelowLevel(t *testing.T) {
	calls := 0
	l := New(LevelInfo, &bytes.Buffer{}).WithRedactor(testRedactor(&calls))
	l.Debug("dropped sk-live-9f3Kd8mQ2wXp")
	l.LogBlock(LevelDebug, "🔍", "dropped", "dropped body")
	if calls != 0 {
		t.Errorf("redactor ran %d times for records the level drops", calls)
	}
}

// Forks made from a redacting logger keep the redactor (that is how the
// run.log tee and the per-node field forks stay covered), and the parent
// it forked from is untouched — the process logger scrubs nothing.
func TestWithRedactorForkInheritance(t *testing.T) {
	var parentBuf, childBuf bytes.Buffer
	parent := New(LevelInfo, &parentBuf)
	child := parent.WithRedactor(testRedactor(new(int))).WithWriter(&childBuf)

	child.Info("child sk-live-9f3Kd8mQ2wXp")
	parent.Info("parent sk-live-9f3Kd8mQ2wXp")

	if strings.Contains(childBuf.String(), "sk-live-9f3Kd8mQ2wXp") {
		t.Errorf("the fork wrote the raw value: %q", childBuf.String())
	}
	if !strings.Contains(parentBuf.String(), "sk-live-9f3Kd8mQ2wXp") {
		t.Error("the parent logger must stay unredacted — it never opted in")
	}
}

// A redactor must compose with itself: a site that still redacts eagerly
// (a value that also reaches an event, or a body bounded before logging)
// hands the chokepoint text it already scrubbed, and the second pass is
// a no-op.
func TestWithRedactorIsIdempotentOverEagerPasses(t *testing.T) {
	var buf bytes.Buffer
	l := New(LevelInfo, &buf).WithRedactor(testRedactor(new(int)))
	l.Info("already scrubbed: %s", "__ITERION_SECRET_test__")
	out := buf.String()
	if strings.Count(out, "__ITERION_SECRET_test__") != 1 {
		t.Errorf("a second pass rewrote the placeholder: %q", out)
	}
}

// Nil-safety: WithRedactor on a nil logger, and a nil redactor, are no-ops.
func TestWithRedactorNilSafe(t *testing.T) {
	var l *Logger
	if l.WithRedactor(testRedactor(new(int))) != nil {
		t.Error("WithRedactor on a nil logger must return nil")
	}
	var buf bytes.Buffer
	New(LevelInfo, &buf).WithRedactor(nil).Info("raw sk-live-9f3Kd8mQ2wXp")
	if !strings.Contains(buf.String(), "sk-live-9f3Kd8mQ2wXp") {
		t.Error("a nil redactor must pass text through")
	}
}

// The JSON record's fields are scrubbed as well as its message — a shipper
// reads fields, not the rendered line.
func TestWithRedactorJSONFields(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithFormat(LevelInfo, &buf, FormatJSON).WithRedactor(testRedactor(new(int)))
	l.WithError(errForTest("dial failed: sk-live-9f3Kd8mQ2wXp")).Info("call sk-live-9f3Kd8mQ2wXp")

	var rec jsonRecord
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not valid JSON: %v: %q", err, buf.String())
	}
	if strings.Contains(rec.Msg, "sk-live-9f3Kd8mQ2wXp") {
		t.Errorf("Msg carries the raw value: %q", rec.Msg)
	}
	if s, _ := rec.Fields["error"].(string); strings.Contains(s, "sk-live-9f3Kd8mQ2wXp") {
		t.Errorf("error field carries the raw value: %q", s)
	}
}

type errForTest string

func (e errForTest) Error() string { return string(e) }
