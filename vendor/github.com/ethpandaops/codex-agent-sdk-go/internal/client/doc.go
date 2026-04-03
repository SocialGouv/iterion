// Package client implements the interactive Client for multi-turn conversations with the agent.
//
// The client package provides a stateful, bidirectional interface to the Codex CLI
// that maintains session state across multiple exchanges. Unlike the one-shot Query()
// function, Client enables:
//   - Multi-turn conversations with persistent context
//   - Interruption of the agent's processing
//   - Dynamic configuration changes (model, permissions)
//   - Hook system integration for tool permission management
//
// The Client uses the protocol package for bidirectional control message handling
// and manages its own goroutines for message reading and routing.
package client
