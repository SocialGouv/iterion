// Package config provides configuration types for the Codex SDK.
package config

import "context"

// Transport defines the interface for Codex CLI communication.
// Implement this to provide custom transports for testing, mocking,
// or alternative communication methods.
//
// The default implementation is CLITransport which spawns a subprocess.
// Custom transports can be injected via Options.Transport.
type Transport interface {
	// Start initializes the transport and prepares it for communication.
	Start(ctx context.Context) error

	// ReadMessages returns channels for receiving messages and errors.
	// The message channel yields parsed JSON objects from the CLI.
	// The error channel yields any errors that occur during reading.
	// Both channels are closed when reading completes or an error occurs.
	ReadMessages(ctx context.Context) (<-chan map[string]any, <-chan error)

	// SendMessage sends a JSON message to the CLI.
	// The data should be a complete JSON message (newline is appended if missing).
	// This method must be safe for concurrent use.
	SendMessage(ctx context.Context, data []byte) error

	// Close terminates the transport and releases resources.
	// It's safe to call Close multiple times.
	Close() error

	// IsReady returns true if the transport is ready for communication.
	IsReady() bool

	// EndInput signals that no more input will be sent.
	// For process-based transports, this typically closes stdin.
	EndInput() error
}
