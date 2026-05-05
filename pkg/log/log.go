// Package log provides a leveled logger with emoji-rich console output
// for the iterion workflow engine.
package log

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
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

// Logger is a leveled logger that writes either emoji-rich human
// output (default) or structured JSON. Loggers may carry a fixed set
// of fields via WithField/WithFields/WithError; the underlying writer
// and mutex are shared between forks so concurrent log lines never
// interleave.
type Logger struct {
	level  Level
	w      io.Writer
	format Format
	mu     *sync.Mutex
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

// NewWithFormat creates a new Logger with an explicit format.
func NewWithFormat(level Level, w io.Writer, format Format) *Logger {
	return &Logger{level: level, w: w, format: format, mu: &sync.Mutex{}}
}

// Nop returns a logger that discards all output (level below error).
func Nop() *Logger {
	return &Logger{level: LevelError - 1, w: io.Discard, mu: &sync.Mutex{}}
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
	msg := fmt.Sprintf(format, args...)

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

	if l.format == FormatJSON {
		fl := l
		if body != "" {
			fl = l.withFields(map[string]any{"body": body})
		}
		fl.writeJSON(level, header)
		return
	}

	ts := time.Now().Format("15:04:05")

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("%s %s %s\n", ts, emoji, header))

	if body != "" {
		lines := strings.Split(body, "\n")
		for _, line := range lines {
			buf.WriteString(blockIndent)
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, buf.String())
}

// Truncate returns s truncated to max bytes with a suffix if it exceeds max.
// Useful for limiting field sizes in log output and events.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// BlockPreview returns s truncated to max bytes, preserving newlines.
// If truncated, a "...[truncated]" marker is appended on a new line.
func BlockPreview(s string, max int) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]"
}
