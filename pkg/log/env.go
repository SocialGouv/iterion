package log

import (
	"io"
	"os"
	"strings"
)

// The environment variables every iterion surface honours. The config
// loader (pkg/config) reads the same names; they are declared here so
// a package with no loader in reach still resolves them identically.
const (
	EnvFormat = "ITERION_LOG_FORMAT"
	EnvLevel  = "ITERION_LOG_LEVEL"
)

// ParseFormat maps a format name onto a Format. Anything other than
// "json" (case-insensitive, trimmed) is the human console format —
// including the empty string, which is how "unset" reaches here.
func ParseFormat(s string) Format {
	if strings.EqualFold(strings.TrimSpace(s), string("json")) {
		return FormatJSON
	}
	return FormatHuman
}

// NewFromEnv builds a logger straight from ITERION_LOG_FORMAT /
// ITERION_LOG_LEVEL, for the surfaces that have no config loader in
// reach: the CLI root before a subcommand runs, and library fallbacks
// that must not stay silent but have not been handed a logger.
//
// Long-running surfaces go through config.LogConfig.NewLogger instead —
// it layers YAML and the per-entry-point default (JSON for the
// server/runner/dispatcher) on top of the same env vars.
func NewFromEnv(w io.Writer) *Logger {
	level, _ := ResolveLevel("", EnvLevel)
	return NewWithFormat(level, w, ParseFormat(os.Getenv(EnvFormat)))
}
