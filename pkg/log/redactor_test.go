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

// Each fork owns its pass slot. A launch surface forks the process logger
// once per run and BuildExecutor installs that run's matcher on the fork,
// so two runs in one process cannot replace each other's — the hazard a
// shared slot would be (iterion#2822).
func TestSetRedactorIsPerFork(t *testing.T) {
	const valueA = "sk-live-9f3Kd8mQ2wXp"
	const valueB = "sk-live-0000AAAA1111"
	var procBuf, bufA, bufB bytes.Buffer
	process := New(LevelInfo, &procBuf)
	// The two shapes a surface uses: a per-run tee (WithWriter), and
	// WithOwnRedactor where there is nothing to tee.
	runA := process.WithWriter(&bufA)
	runB := process.WithOwnRedactor().WithWriter(&bufB)

	runA.SetRedactor(func(s string) string { return strings.ReplaceAll(s, valueA, "__A__") })
	runB.SetRedactor(func(s string) string { return strings.ReplaceAll(s, valueB, "__B__") })

	line := "snapshot: " + valueA + " and " + valueB
	runA.Info("%s", line)
	runB.Info("%s", line)
	process.Info("%s", line)

	if got := bufA.String(); !strings.Contains(got, "__A__") || strings.Contains(got, valueA) {
		t.Errorf("run A's own value survived its own matcher: %q", got)
	}
	if got := bufB.String(); !strings.Contains(got, "__B__") || strings.Contains(got, valueB) {
		t.Errorf("run B's own value survived its own matcher: %q", got)
	}
	// Each run keeps ITS matcher: B's install never reached A's slot.
	if got := bufA.String(); strings.Contains(got, "__B__") {
		t.Errorf("run B's matcher ran on run A's line: %q", got)
	}
	if got := bufB.String(); strings.Contains(got, "__A__") {
		t.Errorf("run A's matcher ran on run B's line: %q", got)
	}
	if got := procBuf.String(); strings.Contains(got, "__A__") || strings.Contains(got, "__B__") {
		t.Errorf("a run's matcher landed on the process logger: %q", got)
	}
}

// SetRedactor is a LATE install: the forks taken afterwards (the engine,
// the executor, the per-node field forks) carry the run's matcher, and
// the parent the handle was forked from never does.
func TestSetRedactorReachesTheForksTakenAfterIt(t *testing.T) {
	const value = "sk-live-9f3Kd8mQ2wXp"
	var procBuf, runBuf bytes.Buffer
	process := New(LevelInfo, &procBuf)
	run := process.WithWriter(&runBuf)
	run.SetRedactor(func(s string) string { return strings.ReplaceAll(s, value, "__RUN__") })

	run.WithField("node", "implement").Info("output: %s", value)
	run.LogBlock(LevelInfo, "📋", "header "+value, "body "+value)
	process.Info("process: %s", value)

	if got := runBuf.String(); strings.Contains(got, value) {
		t.Errorf("a fork taken after the install wrote the raw value: %q", got)
	}
	if strings.Count(runBuf.String(), "__RUN__") != 3 {
		t.Errorf("want the pass on the line, the block header and its body: %q", runBuf.String())
	}
	if got := procBuf.String(); !strings.Contains(got, value) {
		t.Errorf("the parent must stay unredacted — it never opted in: %q", got)
	}
}

// A second install on the SAME handle means two runs reached one handle:
// the last one wins (only the run about to log can protect itself) and
// the collision is announced. A child run installing its own matcher on a
// fork of its parent's logger is not a collision.
func TestSetRedactorAnnouncesASecondInstallOnOneHandle(t *testing.T) {
	const marker = "a per-run logger must be a fork of its own (iterion#2822)"
	var shared bytes.Buffer
	l := New(LevelInfo, &shared)
	l.SetRedactor(func(s string) string { return "A:" + s })
	if strings.Contains(shared.String(), marker) {
		t.Fatalf("the FIRST install must be silent: %q", shared.String())
	}
	l.SetRedactor(func(s string) string { return "B:" + s })
	if !strings.Contains(shared.String(), marker) {
		t.Errorf("a second install on one handle must announce itself: %q", shared.String())
	}
	l.Info("line")
	if got := shared.String(); !strings.Contains(got, "B:line") {
		t.Errorf("the last install must win: %q", got)
	}

	var parentBuf, childBuf bytes.Buffer
	parent := New(LevelInfo, &parentBuf)
	parent.SetRedactor(func(s string) string { return "P:" + s })
	child := parent.WithOwnRedactor().WithWriter(&childBuf)
	child.SetRedactor(func(s string) string { return "C:" + s })
	if strings.Contains(parentBuf.String(), marker) || strings.Contains(childBuf.String(), marker) {
		t.Errorf("a child run's own install on its own fork is not a collision: parent=%q child=%q",
			parentBuf.String(), childBuf.String())
	}
}

// Redact is the eager pass for the one shape the chokepoint cannot serve:
// text a producer must CUT before logging (pkg/supervise cuts a steer at
// 120 B). Cutting first hands the matcher half a value, which it cannot
// recognise.
func TestRedactIsTheEagerPassForTextThatMustBeCut(t *testing.T) {
	const value = "sk-live-9f3Kd8mQ2wXp"
	var buf bytes.Buffer
	l := New(LevelInfo, &buf).WithRedactor(testRedactor(new(int)))
	// The value straddles the cut: 110 bytes of prose, then the value.
	text := strings.Repeat("x", 110) + value + " tail"

	cutFirst := text[:120]
	if !strings.Contains(l.Redact(cutFirst), value[:10]) {
		t.Fatal("the premise of this test is wrong: a cut value must survive the pass")
	}
	scrubbed := l.Redact(text)
	passFirst := scrubbed[:120]
	if strings.Contains(passFirst, value[:10]) {
		t.Errorf("pass-then-cut still shows the value's head: %q", passFirst)
	}
	// The cut now falls inside the PLACEHOLDER, which is the point: what
	// crosses the boundary is the marker, never the value.
	if !strings.Contains(passFirst, "__ITERION_") {
		t.Errorf("pass-then-cut kept no placeholder: %q", passFirst)
	}

	// The record's own pass over already-scrubbed text is a no-op.
	l.Info("%s", scrubbed)
	if strings.Count(buf.String(), "__ITERION_SECRET_test__") != 1 {
		t.Errorf("the record's pass rewrote the placeholder: %q", buf.String())
	}

	var nilLogger *Logger
	if got := nilLogger.Redact(value); got != value {
		t.Errorf("Redact on a nil logger = %q, want the text unchanged", got)
	}
	if got := New(LevelInfo, &buf).Redact(value); got != value {
		t.Errorf("Redact on a logger that scrubs nothing = %q, want the text unchanged", got)
	}
}
