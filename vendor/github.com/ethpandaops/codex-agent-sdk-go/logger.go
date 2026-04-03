package codexsdk

import (
	"io"
	"log/slog"
)

// NopLogger returns a logger that discards all output.
// Use this when you want silent operation with no logging overhead.
func NopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
