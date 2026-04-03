// Package subprocess provides subprocess-based transport for the Codex CLI.
//
// This package implements the Transport interface by spawning the Codex CLI
// as a child process and communicating via stdin/stdout. It handles process
// lifecycle management, message buffering, and error handling.
package subprocess
