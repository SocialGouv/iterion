// Package claudesdk provides a Go SDK for the Claude Code CLI.
//
// The SDK spawns the Claude Code CLI as a subprocess and communicates via
// NDJSON over stdin/stdout. It supports one-shot prompts, multi-turn sessions,
// streaming responses, hooks, MCP servers, and subagents.
//
// This is an inlined fork of github.com/partio-io/claude-agent-sdk-go,
// flattened into a single package for direct integration.
package claudesdk
