package delegate

import (
	"encoding/json"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// BackendGrok is the registration name for the xAI Grok Build CLI backend
// (`backend: "grok"` in the DSL). It drives the official `grok` agent CLI
// (Grok Build TUI / coding agent), whose headless protocol is disjoint from
// claude-code's Session mode:
//
//	grok -p <prompt> --output-format json \
//	     --permission-mode bypassPermissions --always-approve \
//	     [-m <model>] [--rules <system>] [--reasoning-effort <level>]
//
// Credentials and model access are resolved by the CLI itself (OAuth / xAI
// account config under ~/.grok/), so ResolveEnv is nil — same posture as
// kimi. See ADR-065.
const BackendGrok = "grok"

// grokProtocol describes the xAI Grok Build CLI headless invocation.
//
// System prompt delivery uses `--rules` (append to the CLI's native agentic
// system prompt) rather than `--system-prompt-override` (which would strip
// the built-in tool/posture baseline — the same trap as claude_code's
// `--system-prompt` vs `--append-system-prompt`). That pairs with
// SystemPromptAppendToNative for this backend.
//
// Non-interactive tool approval is forced via ExtraArgs so a headless
// studio/CI run never blocks on a permission prompt.
var grokProtocol = CLIAgentProtocol{
	Name:             BackendGrok,
	DefaultBinary:    "grok",
	PromptFlag:       "-p",
	OutputFormatFlag: "--output-format",
	// `json` yields a single envelope {text, sessionId, usage, …} which is
	// simpler to parse than streaming-json for the end-of-run path (the
	// CLIAgentBackend only reads stdout after exit). streaming-json remains
	// accepted by parseGrokOutput as a fallback.
	OutputFormat:     "json",
	ModelFlag:        "-m",
	MapModel:         grokMapModel,
	SystemPromptFlag: "--rules",
	MapEffort:        grokMapEffort,
	ParseOutput:      parseGrokOutput,
	ExtraArgs: []string{
		"--permission-mode", "bypassPermissions",
		"--always-approve",
	},
	// ResolveEnv nil: grok reads its own credentials from ~/.grok / OAuth.
}

// NewGrokBackend constructs the xAI Grok Build CLI backend. command overrides
// the default `grok` binary (a pinned build / wrapper path); empty uses the
// binary on PATH (typically ~/.grok/bin/grok after a Grok Build install).
func NewGrokBackend(logger *iterlog.Logger, command string) *CLIAgentBackend {
	return &CLIAgentBackend{
		Protocol: grokProtocol,
		Command:  command,
		Logger:   logger,
	}
}

// grokMapModel translates an iterion model spec into a Grok CLI model id.
// Bare ids (`grok-4.5`, `grok-4.5-build`) pass through. Known routing
// prefixes (`xai/…`, `grok/…`) are stripped so authors can write either
// `model: "grok-4.5"` or `model: "xai/grok-4.5"` without the CLI rejecting
// an unknown name.
func grokMapModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if idx := strings.Index(model, "/"); idx > 0 {
		prefix := strings.ToLower(model[:idx])
		if prefix == "xai" || prefix == "grok" {
			return strings.TrimSpace(model[idx+1:])
		}
	}
	return model
}

// grokMapEffort maps iterion's reasoning_effort dial onto Grok's
// `--reasoning-effort` flag. Unknown/empty levels are dropped (CLI default).
// `ultracode` is remapped to `high` — Grok has no ultracode mode; the
// orchestration half of ultracode is Anthropic-only (see docs/ultracode.md).
func grokMapEffort(effort string) []string {
	effort = strings.TrimSpace(strings.ToLower(effort))
	switch effort {
	case "":
		return nil
	case "ultracode":
		effort = "high"
	}
	return []string{"--reasoning-effort", effort}
}

// parseGrokOutput extracts the assistant's final text, session id and token
// usage from Grok Build CLI stdout. It accepts both output formats:
//
//   - `json` (preferred): one envelope
//     {"text":"…","sessionId":"…","usage":{"input_tokens":N,"output_tokens":M},…}
//   - `streaming-json`: NDJSON events
//     {"type":"text","data":"…"} / {"type":"thought","data":"…"} /
//     {"type":"end","sessionId":"…","usage":{…},"stopReason":"…"}
//
// Unknown shapes fall back to the raw stdout so the shared schema-aware
// parseSDKOutput can still try to recover a structured result.
func parseGrokOutput(stdout string) (text, sessionID string, tokens int) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return "", "", 0
	}

	// Fast path: whole stdout is a single JSON envelope (output-format json).
	if trimmed[0] == '{' {
		var env grokJSONEnvelope
		if err := json.Unmarshal([]byte(trimmed), &env); err == nil && (env.Text != "" || env.SessionID != "") {
			tokens = env.Usage.InputTokens + env.Usage.OutputTokens
			return env.Text, env.SessionID, tokens
		}
	}

	// Streaming-json path: concatenate type=text data fragments; end event
	// carries sessionId + usage.
	var textBuf strings.Builder
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil || ev == nil {
			continue
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "text":
			if d, ok := ev["data"].(string); ok {
				textBuf.WriteString(d)
			}
		case "end":
			if sid, ok := ev["sessionId"].(string); ok && sid != "" {
				sessionID = sid
			}
			// Also accept snake_case if a future release aligns with claude-code.
			if sid, ok := ev["session_id"].(string); ok && sid != "" {
				sessionID = sid
			}
			if u, ok := ev["usage"].(map[string]any); ok {
				tokens += asInt(u["input_tokens"]) + asInt(u["output_tokens"])
			}
		}
	}
	if textBuf.Len() > 0 || sessionID != "" {
		return textBuf.String(), sessionID, tokens
	}
	return stdout, sessionID, tokens
}

// grokJSONEnvelope is the shape of `--output-format json` stdout.
type grokJSONEnvelope struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionId"`
	Usage     struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}
