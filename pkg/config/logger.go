package config

import (
	"io"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ResolveLevel maps the validated level string onto a pkg/log level.
// Validation upstream guarantees only the five known names reach this
// path, so the fallback to info is purely defensive — a typo must not
// break the boot of a daemon.
func (c LogConfig) ResolveLevel() iterlog.Level {
	if l, err := iterlog.ParseLevel(c.Level); err == nil {
		return l
	}
	return iterlog.LevelInfo
}

// ResolveFormat maps the validated format onto a pkg/log format. Same
// defensive fallback as ResolveLevel.
func (c LogConfig) ResolveFormat() iterlog.Format {
	if c.Format == LogFormatJSON {
		return iterlog.FormatJSON
	}
	return iterlog.FormatHuman
}

// NewLogger builds the logger a surface should run with. Every
// production entry point (server, runner, dispatcher) goes through it,
// so "which format does this process default to" is answered once, by
// the DefaultLogFormat the entry point passes to Load.
func (c LogConfig) NewLogger(w io.Writer) *iterlog.Logger {
	return iterlog.NewWithFormat(c.ResolveLevel(), w, c.ResolveFormat())
}
