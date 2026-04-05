// Package coding provides a coding agent built on goai/agent.
// It wraps the generic agent loop with coding-specific tools (bash, file ops,
// grep, glob, web), context assembly (git, sysinfo, CLAUDE.md), and
// coding-aware permission modes.
package coding

import (
	"context"
	"encoding/json"

	"github.com/SocialGouv/iterion/claw/agent"
	"github.com/SocialGouv/iterion/claw/codingctx"
	"github.com/SocialGouv/iterion/claw/tools"
	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
)

const systemPromptBase = `You are an AI coding assistant. You have access to tools for running bash commands, reading and writing files, searching with glob patterns, and grepping for patterns in code. Use these tools to help with software engineering tasks.`

// CodingAgentConfig configures a CodingAgent.
type CodingAgentConfig struct {
	// Model is the language model to use.
	Model provider.LanguageModel

	// WorkDir is the working directory for tools (bash cwd, context assembly).
	WorkDir string

	// SystemPrompt overrides the default system prompt base. When set, it
	// replaces systemPromptBase entirely (coding context is still appended).
	// When empty, the built-in systemPromptBase is used.
	SystemPrompt string

	// PermissionMode controls tool permissions: "bypass", "default", "accept_edits", "plan".
	PermissionMode string

	// AllowedTools filters which tools are available (empty = all).
	AllowedTools []string

	// ExtraTools are additional tools (e.g. MCP tools from iterion).
	ExtraTools []goai.Tool

	// SessionDir is the directory for session persistence. Empty disables.
	SessionDir string

	// SessionID resumes an existing session (empty = new session).
	SessionID string

	// CompactionMaxTokens is the context window size for compaction decisions.
	CompactionMaxTokens int

	// Hooks (optional).
	OnTextDelta func(string)
	OnToolStart func(string, json.RawMessage)
	OnToolDone  func(string, string, bool)

	// OnAskUser is called when the agent needs user input. If nil, ask_user
	// returns a fallback message.
	OnAskUser func(question string) string
}

// CodingAgent wraps goai/agent.Agent with coding-specific configuration.
type CodingAgent struct {
	agent *agent.Agent
}

// New creates a CodingAgent with coding tools and context assembly.
func New(cfg CodingAgentConfig) (*CodingAgent, error) {
	// Build coding tools.
	codingTools := buildToolSet(cfg.WorkDir, cfg.AllowedTools, cfg.OnAskUser)

	// Merge with extra tools.
	allTools := make([]goai.Tool, 0, len(codingTools)+len(cfg.ExtraTools))
	allTools = append(allTools, codingTools...)
	allTools = append(allTools, cfg.ExtraTools...)

	// Assemble system prompt with coding context.
	systemPrompt := cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = systemPromptBase
	}
	ctxAssembler := codingctx.NewAssembler(cfg.WorkDir)
	if ctx := ctxAssembler.Assemble(); ctx != "" {
		systemPrompt += "\n\n" + ctx
	}

	// Build permission hook.
	permHook := buildPermissionHook(cfg.PermissionMode)

	a, err := agent.New(agent.Config{
		Model:               cfg.Model,
		SystemPrompt:        systemPrompt,
		Tools:               allTools,
		SessionDir:          cfg.SessionDir,
		SessionID:           cfg.SessionID,
		CompactionMaxTokens: cfg.CompactionMaxTokens,
		OnPermissionCheck:   permHook,
		OnTextDelta:         cfg.OnTextDelta,
		OnToolStart:         cfg.OnToolStart,
		OnToolDone:          cfg.OnToolDone,
	})
	if err != nil {
		return nil, err
	}

	return &CodingAgent{agent: a}, nil
}

// Run executes the agent loop with the given prompt.
func (c *CodingAgent) Run(ctx context.Context, prompt string) (*agent.Result, error) {
	return c.agent.Run(ctx, prompt)
}

// RunStreaming executes the agent loop, emitting events.
func (c *CodingAgent) RunStreaming(ctx context.Context, prompt string, events chan<- agent.Event) (*agent.Result, error) {
	return c.agent.RunStreaming(ctx, prompt, events)
}

// SessionID returns the current session identifier.
func (c *CodingAgent) SessionID() string {
	return c.agent.SessionID()
}

// Close persists session state.
func (c *CodingAgent) Close() error {
	return c.agent.Close()
}

// buildToolSet creates the standard coding tool set, optionally filtered.
func buildToolSet(workDir string, allowedTools []string, onAskUser func(string) string) []goai.Tool {
	all := []goai.Tool{
		tools.Bash(workDir),
		tools.ReadFile(workDir),
		tools.WriteFile(workDir),
		tools.FileEdit(workDir),
		tools.Glob(workDir),
		tools.Grep(workDir),
		tools.WebFetch(),
		tools.WebSearch(),
		tools.AskUser(onAskUser),
		tools.TodoWrite(workDir),
	}

	if len(allowedTools) == 0 {
		return all
	}

	// Filter to only allowed tools.
	allowed := make(map[string]bool, len(allowedTools))
	for _, name := range allowedTools {
		allowed[name] = true
	}

	var filtered []goai.Tool
	for _, t := range all {
		if allowed[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// buildPermissionHook creates a permission hook based on the mode string.
func buildPermissionHook(mode string) func(string, json.RawMessage) agent.PermissionDecision {
	switch mode {
	case "bypass", "":
		return nil // nil = allow all
	case "plan":
		return func(_ string, _ json.RawMessage) agent.PermissionDecision {
			return agent.PermDeny // plan mode: describe but don't execute
		}
	case "accept_edits":
		// Allow file operations, ask for everything else.
		readOnly := map[string]bool{
			"read_file": true, "glob": true, "grep": true,
			"web_fetch": true, "web_search": true, "todo_write": true,
			"ask_user": true,
		}
		return func(toolName string, _ json.RawMessage) agent.PermissionDecision {
			if readOnly[toolName] {
				return agent.PermAllow
			}
			// file_edit, write_file allowed (accept edits mode)
			if toolName == "file_edit" || toolName == "write_file" {
				return agent.PermAllow
			}
			return agent.PermAsk
		}
	default: // "default"
		readOnly := map[string]bool{
			"read_file": true, "glob": true, "grep": true,
			"web_fetch": true, "web_search": true, "todo_write": true,
			"ask_user": true,
		}
		return func(toolName string, _ json.RawMessage) agent.PermissionDecision {
			if readOnly[toolName] {
				return agent.PermAllow
			}
			return agent.PermAsk
		}
	}
}
