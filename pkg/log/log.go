// Package log provides a leveled logger with emoji-rich console output
// for the iterion workflow engine.
package log

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level represents a logging verbosity level.
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
	LevelTrace
)

// String returns the human-readable name of the level.
func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	case LevelTrace:
		return "trace"
	default:
		return "unknown"
	}
}

// ParseLevel converts a string to a Level. Case-insensitive.
// Returns LevelInfo if the string is empty.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info", "":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q (valid: error, warn, info, debug, trace)", s)
	}
}

// ResolveLevel resolves the log level from explicit value, env var fallback,
// and default. The explicit value takes precedence over the env var.
func ResolveLevel(explicit string, envVar string) (Level, error) {
	if explicit != "" {
		return ParseLevel(explicit)
	}
	if v := os.Getenv(envVar); v != "" {
		return ParseLevel(v)
	}
	return LevelInfo, nil
}

// Format selects between the human-readable console format and the
// structured JSON format used by the cloud-mode server / runner.
type Format int

const (
	// FormatHuman emits the legacy "HH:MM:SS emoji message" line. This
	// is the default and preserves byte-for-byte compatibility with
	// pre-cloud iterion output.
	FormatHuman Format = iota
	// FormatJSON emits one JSON object per line; see jsonRecord for the
	// schema. Suitable for shipping logs into Loki / ELK / CloudWatch.
	FormatJSON
)

// Hook receives every record a Logger emits at or above warn level,
// with the record's inherited fields as a private copy. It is the seam
// error trackers ride: pkg/errtrack registers one so an error line
// becomes a tracker event and a warn line a breadcrumb.
//
// Contract for implementations:
//   - MUST NOT block — the hook runs synchronously on the logging
//     goroutine (ordering matters: a breadcrumb has to reach the
//     tracker before the event it explains).
//   - MAY panic; the dispatch recovers, so a broken hook degrades to a
//     single "hook panicked" line instead of taking the process down.
//   - MUST NOT log through the same Logger — the recovery path writes
//     directly to the underlying writer, but a non-panicking hook that
//     logs at warn+ would recurse without bound.
type Hook func(level Level, msg string, fields map[string]any)

// hookThreshold is the least severe level a Hook is invoked for.
// Levels are ordered most-severe-first (LevelError == 0), so "at or
// above warn" reads as `level <= hookThreshold`.
const hookThreshold = LevelWarn

// redactFn is one redaction pass over a record the logger keeps. An
// alias, so every exported signature stays `func(string) string`.
type redactFn = func(string) string

// redactPass is what a redactor slot holds: the pass, and whether it was
// INSTALLED on that very handle (SetRedactor) rather than inherited when
// the handle was forked. The second field is what tells a per-run install
// apart from a seed, so the collision SetRedactor announces is the real
// one — two runs on one handle — and not a child run installing its own
// matcher on a fork of its parent's logger.
type redactPass struct {
	fn        redactFn
	installed bool
}

// Logger is a leveled logger that writes either emoji-rich human
// output (default) or structured JSON. Loggers may carry a fixed set
// of fields via WithField/WithFields/WithError; the underlying writer,
// mutex and hook are shared between forks so concurrent log lines
// never interleave and a hook installed on the root logger is seen by
// every fork made from it, before or after SetHook. The redaction pass
// is the one piece a fork does NOT share: it is seeded from the parent
// and settable per handle (SetRedactor), because it carries ONE run's
// secret matcher.
type Logger struct {
	level  Level
	w      io.Writer
	format Format
	mu     *sync.Mutex
	// hook is the shared, atomically-swappable warn+ callback slot.
	// Shared (not copied) across forks so SetHook on any handle to the
	// tree reaches all of them. Never nil for a Logger built through a
	// constructor; the dispatch stays nil-safe regardless.
	hook *atomic.Pointer[Hook]
	// redact is this handle's OWN pass slot: what scrubs every record the
	// logger KEEPS — line, block header and body, and string field values
	// — before the write and before the hook dispatch. Each fork allocates
	// its own slot seeded with the parent's pass, and SetRedactor writes
	// only the slot of the handle it is called on, so two runs forked from
	// one process logger cannot replace each other's secret matcher
	// (iterion#2822). nil on a Logger built outside the constructors; the
	// read and both writers stay nil-safe.
	redact *atomic.Pointer[redactPass]
	// fields is the inherited context. nil for the root logger; copied
	// (not aliased) on each WithField call so a fork's mutations don't
	// leak back to the parent.
	fields map[string]any
}

// New creates a new Logger with the given level and writer in the
// human-readable format. Equivalent to NewWithFormat(level, w, FormatHuman).
func New(level Level, w io.Writer) *Logger {
	return NewWithFormat(level, w, FormatHuman)
}

// NewWithFormat creates a new Logger with an explicit format. A nil
// writer is treated as io.Discard so callers using the "silence all
// output" idiom (passing nil) don't panic on the first log line.
func NewWithFormat(level Level, w io.Writer, format Format) *Logger {
	if w == nil {
		w = io.Discard
	}
	return &Logger{level: level, w: w, format: format, mu: &sync.Mutex{}, hook: &atomic.Pointer[Hook]{}, redact: &atomic.Pointer[redactPass]{}}
}

// Nop returns a logger that discards all output (level below error).
func Nop() *Logger {
	return &Logger{level: LevelError - 1, w: io.Discard, mu: &sync.Mutex{}, hook: &atomic.Pointer[Hook]{}, redact: &atomic.Pointer[redactPass]{}}
}

// SetHook installs h as the warn+ callback for l and every fork made
// from it (forks share the slot). A nil h clears the hook. Safe on a
// nil Logger.
func (l *Logger) SetHook(h Hook) {
	if l == nil || l.hook == nil {
		return
	}
	if h == nil {
		l.hook.Store(nil)
		return
	}
	l.hook.Store(&h)
}

// dispatchHook invokes the installed hook for a record the logger has
// already emitted. Called outside the writer mutex so a slow hook
// never stalls another goroutine's log line.
func (l *Logger) dispatchHook(level Level, msg string) {
	if l.hook == nil || level > hookThreshold {
		return
	}
	p := l.hook.Load()
	if p == nil || *p == nil {
		return
	}
	h := *p
	// A panicking hook degrades to one line on the underlying writer.
	// emit (not log) so the report can't re-enter the dispatch.
	defer func() {
		if r := recover(); r != nil {
			l.emit(LevelError, "❌", fmt.Sprintf("log: hook panicked: %v", r))
		}
	}()
	h(level, msg, l.snapshotFields())
}

// snapshotFields returns a private copy of the inherited context so a
// hook can retain or mutate it without racing the logger tree. String
// values cross the logger's redactor like the record itself: the hook is
// a sink of the same class (the error tracker reads them).
func (l *Logger) snapshotFields() map[string]any {
	if len(l.fields) == 0 {
		return nil
	}
	out := make(map[string]any, len(l.fields))
	for k, v := range l.fields {
		if s, ok := v.(string); ok {
			v = l.redactString(s)
		}
		out[k] = v
	}
	return out
}

// Level returns the configured log level.
func (l *Logger) Level() Level {
	if l == nil {
		return LevelInfo
	}
	return l.level
}

// Writer returns the underlying io.Writer. Useful for callers that
// need to compose new loggers that tee output to additional sinks
// (e.g. a per-run buffer) without losing the original destination.
func (l *Logger) Writer() io.Writer {
	if l == nil {
		return io.Discard
	}
	return l.w
}

// IsEnabled returns true if the given level would produce output.
func (l *Logger) IsEnabled(level Level) bool {
	if l == nil {
		return false
	}
	return level <= l.level
}

// Error logs at error level with ❌ prefix.
func (l *Logger) Error(format string, args ...any) {
	l.log(LevelError, "❌", format, args...)
}

// Warn logs at warn level with ⚠️  prefix.
func (l *Logger) Warn(format string, args ...any) {
	l.log(LevelWarn, "⚠️ ", format, args...)
}

// Info logs at info level with a contextual prefix (caller chooses emoji).
func (l *Logger) Info(format string, args ...any) {
	l.log(LevelInfo, "ℹ️ ", format, args...)
}

// Debug logs at debug level with 🔍 prefix.
func (l *Logger) Debug(format string, args ...any) {
	l.log(LevelDebug, "🔍", format, args...)
}

// Trace logs at trace level with 🔬 prefix.
func (l *Logger) Trace(format string, args ...any) {
	l.log(LevelTrace, "🔬", format, args...)
}

// Logf logs a pre-formatted message at the given level with a custom emoji prefix.
// This is useful when callers want to choose their own emoji.
func (l *Logger) Logf(level Level, emoji string, format string, args ...any) {
	l.log(level, emoji, format, args...)
}

func (l *Logger) log(level Level, emoji string, format string, args ...any) {
	if l == nil || level > l.level {
		return
	}
	msg := l.redactString(fmt.Sprintf(format, args...))
	l.emit(level, emoji, msg)
	l.dispatchHook(level, msg)
}

// emit writes one already-formatted record in the logger's format. It
// performs no level check and no hook dispatch — callers own both.
func (l *Logger) emit(level Level, emoji string, msg string) {
	if l.format == FormatJSON {
		l.writeJSON(level, msg)
		return
	}

	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("%s %s %s\n", ts, emoji, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, line)
}

// WithRedactor returns a fork of l that passes every record it KEEPS
// through redact — the formatted line, a LogBlock's header and body, and
// the string values of its inherited fields — before the record is written
// and before the warn+ hook is dispatched, so every sink of the logger
// (console, run.log tee, error tracking) sees the same scrubbed text. The
// scan runs AFTER the level test: a record the logger drops is never
// redacted, so a quiet log pays nothing. A nil redact is a no-op fork.
// Safe on a nil Logger.
//
// This is the redaction chokepoint a RUN's logger carries
// (runview.BuildExecutor wires the run's secret guard here): ten
// per-sink passes would leave the eleventh to drift, so producers hand
// third-party text to the logger raw and the fork scrubs it once. Text a
// caller TRUNCATES before logging still owes its own pass first — the
// chokepoint can only recognise a value the cut kept whole, which is what
// Redact is for.
//
// The fork's pass lands in the fork's OWN slot: l keeps what it carried,
// and a later SetRedactor on either handle leaves the other alone.
func (l *Logger) WithRedactor(redact func(string) string) *Logger {
	if l == nil {
		return nil
	}
	out := l.withFields(nil)
	out.storeRedactor(redact, false)
	return out
}

// SetRedactor installs redact on THIS handle's slot, and so on every fork
// made from it afterwards. It is the late install a launch surface owes a
// run whose secret matcher does not exist yet when its logger is built:
// runview.BuildExecutor builds the run's guard from the resolved
// credentials and sets it here, so the lines the surface itself logs on
// that handle — and the engine, executor and supervisor forks taken from
// it — all cross the run's matcher (iterion#2822, docs/secrets.md).
//
// The slot is per-handle: the parent this logger was forked from is never
// touched, and forks taken BEFORE the call keep the pass they were seeded
// with. That is why a per-run logger must be a FORK — a surface that
// hands BuildExecutor the bare process logger installs one run's matcher
// where the next run would replace it.
//
// A SECOND install on the same handle is accepted and announced: only the
// run about to log can protect itself, so the last install wins, and the
// line says what the collision means — two runs reached one handle, a
// surface skipped its fork. A pass merely INHERITED from the parent is
// not a collision: a child run installing its own matcher on a fork of
// its parent's logger is the normal shape. A nil redact clears the slot.
// Safe on a nil Logger and on one built outside the constructors (no
// slot: nothing to set).
func (l *Logger) SetRedactor(redact func(string) string) {
	if l == nil || l.redact == nil {
		return
	}
	prev := l.storeRedactor(redact, true)
	if redact != nil && prev != nil && prev.installed {
		l.Warn("log: a run's redaction pass was already installed on this logger and a second one replaced it — a per-run logger must be a fork of its own (iterion#2822)")
	}
}

// WithOwnRedactor returns a fork of l — same level, writer, format,
// fields and hook — whose redaction slot is its own, seeded with l's
// current pass. A launch surface calls it to give ONE run a slot
// BuildExecutor can set without reaching l or any other run, when it has
// no other reason to fork (no per-run tee, or a tee that failed to open).
// Every other fork (WithWriter, WithField, WithRedactor) owns its slot
// too, so a surface that already forked for its per-run sink owes no
// second call. Safe on a nil Logger.
func (l *Logger) WithOwnRedactor() *Logger {
	if l == nil {
		return nil
	}
	return l.withFields(nil)
}

// Redact runs this logger's pass over s and returns the result. It serves
// the one producer shape the chokepoint cannot: text that must be CUT
// before it is logged. A cut hands the pass half a secret, which no
// matcher recognises, so the producer redacts first and bounds the result
// — the record's own pass is then a no-op (the pass is idempotent over
// placeholders). Safe on a nil Logger and on one that scrubs nothing,
// which return s unchanged.
func (l *Logger) Redact(s string) string {
	return l.redactString(s)
}

// storeRedactor writes the handle's own slot and returns the pass it
// replaced, nil when there was none. installed distinguishes a per-run
// install (SetRedactor) from a fork-time pass.
func (l *Logger) storeRedactor(redact func(string) string, installed bool) *redactPass {
	if redact == nil {
		return l.redact.Swap(nil)
	}
	return l.redact.Swap(&redactPass{fn: redact, installed: installed})
}

// redactString applies the logger's own pass, if one is wired.
func (l *Logger) redactString(s string) string {
	if l == nil || s == "" || l.redact == nil {
		return s
	}
	p := l.redact.Load()
	if p == nil || p.fn == nil {
		return s
	}
	return p.fn(s)
}

// forkRedactorSlot gives a fork its own slot, seeded with the pass the
// parent carries at fork time. The seed is not an install: the fork may
// have its own run's matcher set on it without that reading as a
// collision (SetRedactor).
func forkRedactorSlot(parent *atomic.Pointer[redactPass]) *atomic.Pointer[redactPass] {
	slot := &atomic.Pointer[redactPass]{}
	if parent == nil {
		return slot
	}
	if p := parent.Load(); p != nil && p.fn != nil {
		slot.Store(&redactPass{fn: p.fn})
	}
	return slot
}

// WithWriter returns a fork of l writing to w instead of l's own
// writer, keeping the level, the FORMAT, the inherited fields and the
// hook slot. It is how a per-run logger tees into a buffer without
// leaving the seam: building a fresh Logger there would silently reset
// the format to human (breaking the JSON stream of a runner pod that
// tees into its own stdout) and drop the tracker hook, so a run's
// error lines would never reach error tracking.
//
// The mutex is shared with l, so a fork whose w also writes to l's
// writer still interleaves atomically with the parent's own lines.
func (l *Logger) WithWriter(w io.Writer) *Logger {
	if l == nil {
		return nil
	}
	if w == nil {
		w = io.Discard
	}
	out := l.withFields(nil)
	out.w = w
	return out
}

// WithField returns a fork of l carrying the given (key, value) pair
// in its context. The fork shares the underlying writer and mutex so
// concurrent writes from parent + child interleave atomically.
func (l *Logger) WithField(key string, value any) *Logger {
	if l == nil {
		return nil
	}
	return l.withFields(map[string]any{key: value})
}

// WithFields returns a fork of l carrying every (key, value) pair
// from fields. nil and empty maps return a no-op fork (still safe to
// call on a nil logger).
func (l *Logger) WithFields(fields map[string]any) *Logger {
	if l == nil {
		return nil
	}
	return l.withFields(fields)
}

// WithError returns a fork carrying the given error under the "error"
// field. nil errors are a no-op (the fork has no extra context).
func (l *Logger) WithError(err error) *Logger {
	if l == nil || err == nil {
		return l
	}
	return l.withFields(map[string]any{"error": err.Error()})
}

func (l *Logger) withFields(extra map[string]any) *Logger {
	out := &Logger{
		level:  l.level,
		w:      l.w,
		format: l.format,
		mu:     l.mu,
		hook:   l.hook,
		redact: forkRedactorSlot(l.redact),
	}
	if len(l.fields) == 0 && len(extra) == 0 {
		return out
	}
	merged := make(map[string]any, len(l.fields)+len(extra))
	for k, v := range l.fields {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	out.fields = merged
	return out
}

// blockIndent is the prefix used for continuation lines in LogBlock output.
// It aligns with content after "HH:MM:SS emoji " (9 chars + separator).
const blockIndent = "         │ "

// LogBlock logs a header line followed by a multi-line indented body block.
// The entire output is written in a single mutex-protected write to prevent
// interleaving from concurrent goroutines. If body is empty, only the header
// is printed.
//
// In JSON mode, the body is collapsed into the "body" field of the
// record so downstream tooling sees a single structured event rather
// than an indented blob.
func (l *Logger) LogBlock(level Level, emoji string, header string, body string) {
	if l == nil || level > l.level {
		return
	}

	// Kept records cross the redactor once, here, before either sink
	// (writer and hook) reads them. A body the caller truncated owes its
	// own pass BEFORE the cut — see WithRedactor.
	header = l.redactString(header)
	body = l.redactString(body)

	// The body travels as a field, so the hook sees the same structured
	// record a JSON shipper would.
	fl := l
	if body != "" {
		fl = l.withFields(map[string]any{"body": body})
	}

	if l.format == FormatJSON {
		fl.writeJSON(level, header)
		fl.dispatchHook(level, header)
		return
	}

	ts := time.Now().Format("15:04:05")

	var buf strings.Builder
	fmt.Fprintf(&buf, "%s %s %s\n", ts, emoji, header)

	if body != "" {
		// TrimRight on trailing newlines so a body ending with "\n"
		// doesn't emit a stray indented blank line — strings.Split
		// returns an extra empty element for every trailing separator
		// and the unconditional WriteByte below would otherwise echo it.
		lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
		for _, line := range lines {
			buf.WriteString(blockIndent)
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}

	l.mu.Lock()
	_, _ = io.WriteString(l.w, buf.String())
	l.mu.Unlock()

	fl.dispatchHook(level, header)
}

// Truncate returns s truncated to at most max bytes with a suffix if
// it exceeds max. When the byte boundary falls inside a multi-byte
// UTF-8 sequence (CJK characters, emoji, etc.) the truncation is
// pulled back to the previous rune start so the returned string stays
// valid UTF-8 — otherwise json.Marshal escapes the dangling bytes as
// � and the persisted log field looks garbled downstream.
// Useful for limiting field sizes in log output and events.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return safeByteCut(s, max) + "...[truncated]"
}

// BlockPreview returns s truncated to at most max bytes, preserving
// newlines. The byte cut is rune-safe (see Truncate). If truncated,
// a "...[truncated]" marker is appended on a new line.
func BlockPreview(s string, max int) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return safeByteCut(s, max) + "\n...[truncated]"
}

// BlockPreviewRendered is [BlockPreview] bounded on what [Logger.LogBlock]
// WRITES rather than on the body it is given: every line of a block carries
// blockIndent, which the caller never sees, so a body bounded on its own
// bytes still reaches the log up to eight times over — one byte of text per
// line against thirteen of prefix (measured: a 1 MiB body of single-byte
// lines renders as 7.5 MiB). Callers whose body is third-party text, with
// the third party choosing both its size and where its newlines fall, bound
// it here.
//
// The cut is rune-safe (see [Truncate]), newlines are preserved, and the
// marker lands on its own line — itself inside max, down to a max that only
// fits the marker (below that, the header goes out with no body at all). The
// accounting rounds UP (a trailing newline the renderer trims is still
// charged a line), so the returned body never renders past max and never
// over-reserves by more than one line's prefix.
//
// The accounting is the HUMAN renderer's, the one that carries the prefix. In
// JSON mode the body travels as a record field, which this bound covers for
// what it adds (no indent) but not for what JSON escaping adds on top — the
// property every byte cap in the tree has.
func BlockPreviewRendered(s string, max int) string {
	if s == "" {
		return ""
	}
	// What one body line costs the writer: the continuation prefix plus the
	// newline the renderer puts back.
	const perLine = len(blockIndent) + 1
	const marker = "...[truncated]"
	if renderedBlockSize(s) <= max {
		return s
	}
	// Not even one prefixed line fits: a marker nobody asked for would put
	// the block OVER the bound it is here to hold.
	if max < perLine+len(marker) {
		return ""
	}
	budget := max - (perLine + len(marker))
	used, end := perLine, 0
	for end < len(s) {
		step := 1
		if s[end] == '\n' {
			step = perLine
		}
		if used+step > budget {
			break
		}
		used += step
		end++
	}
	if end == 0 {
		return marker
	}
	return safeByteCut(s, end) + "\n" + marker
}

// renderedBlockSize is how many bytes [Logger.LogBlock] writes for body s,
// excluding its header line.
func renderedBlockSize(s string) int {
	if s == "" {
		return 0
	}
	lines := strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
	return len(strings.TrimRight(s, "\n")) - (lines - 1) + lines*(len(blockIndent)+1)
}

// safeByteCut returns s[:max] pulled back to the nearest preceding
// rune boundary so the returned string is always valid UTF-8.
// Continuation bytes have the bit pattern 10xxxxxx; we walk back
// while we are on one. The worst case is three bytes (4-byte UTF-8
// codepoint cut after its leading byte), bounded.
func safeByteCut(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if max >= len(s) {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
