// Package agent provides an agentic loop abstraction on top of goai's
// LanguageModel. It handles multi-turn tool execution, streaming events,
// permission hooks, session persistence, and automatic context compaction.
package agent

import (
	"encoding/json"

	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
)

// Config configures an Agent.
type Config struct {
	// Model is the language model to use for generation.
	Model provider.LanguageModel

	// SystemPrompt is injected as the system message for every turn.
	SystemPrompt string

	// Tools available to the agent. These are unified goai.Tool instances
	// that may come from any source (built-in, MCP, custom).
	Tools []goai.Tool

	// MaxOutputTokens limits each generation response. 0 means provider default.
	MaxOutputTokens int

	// MaxSteps limits the total number of generation steps (0 = unlimited).
	MaxSteps int

	// SessionDir is the directory for session persistence. Empty disables persistence.
	SessionDir string

	// SessionID resumes an existing session. Empty starts a new session.
	SessionID string

	// CompactionThreshold is the ratio of estimated tokens to MaxOutputTokens
	// that triggers automatic compaction (default 0.75).
	CompactionThreshold float64

	// CompactionKeepRecent is the number of recent messages retained verbatim
	// after compaction (default 10).
	CompactionKeepRecent int

	// CompactionMaxTokens is the assumed context window size for compaction
	// decisions. If 0, compaction is disabled.
	CompactionMaxTokens int

	// ProviderOptions are passed through to every generation call.
	ProviderOptions map[string]any

	// Headers are additional HTTP headers for every generation call.
	Headers map[string]string

	// PromptCaching enables provider-specific prompt caching.
	PromptCaching bool

	// --- Hooks ---

	// OnTextDelta is called for each text chunk during streaming.
	OnTextDelta func(text string)

	// OnToolStart is called when a tool execution begins.
	OnToolStart func(name string, input json.RawMessage)

	// OnToolDone is called when a tool execution completes.
	OnToolDone func(name string, result string, isError bool)

	// OnUsage is called with token usage after each generation step.
	OnUsage func(usage provider.Usage)

	// OnStepFinish is called after each complete step (generation + tool execution).
	OnStepFinish func(step StepInfo)

	// OnPermissionCheck is called before each tool execution. If nil, all tools
	// are allowed. Return PermDeny to block, PermAllow to proceed, PermAsk to
	// request interactive confirmation (the agent will emit an EventPermissionAsk).
	OnPermissionCheck func(toolName string, input json.RawMessage) PermissionDecision
}

// PermissionDecision is the result of a permission check.
type PermissionDecision int

const (
	// PermAllow permits the tool execution.
	PermAllow PermissionDecision = iota
	// PermDeny blocks the tool execution.
	PermDeny
	// PermAsk requests interactive confirmation via EventPermissionAsk.
	PermAsk
)

// StepInfo describes a completed step in the agent loop.
type StepInfo struct {
	// Number is the 1-based step index.
	Number int

	// Text generated in this step.
	Text string

	// ToolCalls made in this step.
	ToolCalls []provider.ToolCall

	// Usage for this step.
	Usage provider.Usage

	// FinishReason for this step's generation.
	FinishReason provider.FinishReason
}

// Result is the final output of an agent run.
type Result struct {
	// Text is the accumulated text from all steps.
	Text string

	// Usage is the aggregated token usage across all steps.
	Usage provider.Usage

	// SessionID is the session identifier (for resume).
	SessionID string

	// Steps contains info for each completed step.
	Steps []StepInfo
}

// applyDefaults fills in zero-value config fields with sensible defaults.
func (c *Config) applyDefaults() {
	if c.CompactionThreshold == 0 {
		c.CompactionThreshold = 0.75
	}
	if c.CompactionKeepRecent == 0 {
		c.CompactionKeepRecent = 10
	}
}
