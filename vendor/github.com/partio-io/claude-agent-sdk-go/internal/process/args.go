package process

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Config holds parameters that map to CLI flags.
type Config struct {
	Model              string
	SystemPrompt       string
	AppendSystemPrompt string
	Cwd                string
	Verbose            bool

	AllowedTools    []string
	DisallowedTools []string
	PermissionMode  string

	MaxTurns     int
	MaxBudgetUSD float64

	IncludePartialMessages bool

	Resume               string
	ForkSession          bool
	ContinueConversation bool
	NoSessionPersistence bool

	OutputFormat map[string]any
	AddDirs      []string

	// MCPConfigJSON is the JSON-encoded MCP server config (written to a temp file).
	MCPConfigJSON []byte

	// AgentsJSON is the JSON-encoded agents config.
	AgentsJSON []byte
}

// BuildArgs converts a Config into CLI arguments for `claude --print`.
func BuildArgs(cfg Config, streaming bool) []string {
	args := []string{"--print"}

	if streaming {
		args = append(args, "--output-format", "stream-json")
		args = append(args, "--input-format", "stream-json")
	} else {
		args = append(args, "--output-format", "stream-json")
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
	}
	if cfg.Verbose {
		args = append(args, "--verbose")
	}
	if cfg.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}

	if len(cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	}
	if len(cfg.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(cfg.DisallowedTools, ","))
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}

	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", cfg.MaxTurns))
	}
	if cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%g", cfg.MaxBudgetUSD))
	}

	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	if cfg.ForkSession {
		args = append(args, "--fork-session")
	}
	if cfg.ContinueConversation {
		args = append(args, "--continue")
	}
	if cfg.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}

	if cfg.OutputFormat != nil {
		if b, err := json.Marshal(cfg.OutputFormat); err == nil {
			args = append(args, "--json-schema", string(b))
		}
	}

	for _, dir := range cfg.AddDirs {
		args = append(args, "--add-dir", dir)
	}

	if len(cfg.AgentsJSON) > 0 {
		args = append(args, "--agents", string(cfg.AgentsJSON))
	}

	return args
}

// BuildMCPConfigJSON creates the JSON for --mcp-config from a map of server configs.
func BuildMCPConfigJSON(servers map[string]any) ([]byte, error) {
	cfg := map[string]any{"mcpServers": servers}
	return json.Marshal(cfg)
}

// JoinSettingSources joins setting sources for the --setting-sources flag.
func JoinSettingSources(sources []string) string {
	return strings.Join(sources, ",")
}
