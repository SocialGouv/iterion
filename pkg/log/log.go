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

// Logger is a leveled logger that writes emoji-rich formatted output.
type Logger struct {
	level Level
	w     io.Writer
	mu    sync.Mutex
}

// New creates a new Logger with the given level and writer.
func New(level Level, w io.Writer) *Logger {
	return &Logger{level: level, w: w}
}

// Nop returns a logger that discards all output (level below error).
func Nop() *Logger {
	return &Logger{level: LevelError - 1, w: io.Discard}
}

// Level returns the configured log level.
func (l *Logger) Level() Level {
	if l == nil {
		return LevelInfo
	}
	return l.level
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
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("%s %s %s\n", ts, emoji, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, line)
}

// blockIndent is the prefix used for continuation lines in LogBlock output.
// It aligns with content after "HH:MM:SS emoji " (9 chars + separator).
const blockIndent = "         │ "

// LogBlock logs a header line followed by a multi-line indented body block.
// The entire output is written in a single mutex-protected write to prevent
// interleaving from concurrent goroutines. If body is empty, only the header
// is printed.
func (l *Logger) LogBlock(level Level, emoji string, header string, body string) {
	if l == nil || level > l.level {
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
