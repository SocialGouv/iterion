package runtime

import (
	"fmt"
	"os/exec"
	"strings"
)

// --- contextLoop interface (context_cmds.go) ---

// ListContextFiles returns the files currently in conversation context.
func (a *LoopAdapter) ListContextFiles() []string {
	if a.loop == nil {
		return nil
	}
	if len(a.focusPaths) == 0 {
		return nil
	}
	// Return a copy to prevent callers from mutating internal state.
	files := make([]string, len(a.focusPaths))
	copy(files, a.focusPaths)
	return files
}

// ContextTokenBreakdown returns approximate token counts by source.
func (a *LoopAdapter) ContextTokenBreakdown() map[string]int {
	if a.loop == nil || a.loop.Session == nil {
		return nil
	}
	breakdown := make(map[string]int)
	msgs := a.loop.Session.Messages
	for _, m := range msgs {
		tokens := 0
		for _, c := range m.Content {
			if c.Type == "text" {
				tokens += len(c.Text) / 4 // rough chars-per-token estimate
			}
		}
		breakdown[m.Role] += tokens
	}
	if a.loop.Session.CompactionSummary != "" {
		breakdown["compaction_summary"] = len(a.loop.Session.CompactionSummary) / 4
	}
	return breakdown
}

// ClearContext clears the context state.
func (a *LoopAdapter) ClearContext() {
	a.focusPaths = nil
}

// --- searchLoop interface (context_cmds.go) ---

// SearchFiles searches the workspace for files matching the query using grep.
func (a *LoopAdapter) SearchFiles(query string) ([]string, error) {
	if a.loop == nil {
		return nil, ErrSubsystemUnavailable
	}
	// Use grep tool via shell for workspace search.
	cmd := exec.Command("grep", "-rl", "--include=*.go", "--include=*.ts", "--include=*.js",
		"--include=*.py", "--include=*.rs", "--include=*.md", query, ".")
	cmd.Dir = a.loop.workspaceRoot()
	out, err := cmd.Output()
	if err != nil {
		// grep returns exit code 1 when no matches found — not an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var results []string
	for _, line := range lines {
		if line != "" {
			results = append(results, line)
		}
	}
	return results, nil
}

// --- rewindLoop interface (context_cmds.go) ---

// RewindSteps removes the last n message pairs from the session.
func (a *LoopAdapter) RewindSteps(n int) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if err := a.requireNoActiveTurn(); err != nil {
		return err
	}
	if a.loop.Session == nil {
		return NewLoopError(LoopErrSubsystemUnavailable, "session", "no active session")
	}
	msgs := a.loop.Session.Messages
	// Each "step" removes 2 messages (user + assistant pair).
	remove := n * 2
	if remove > len(msgs) {
		remove = len(msgs)
	}
	a.loop.Session.Messages = msgs[:len(msgs)-remove]
	return nil
}

// --- clipboardLoop interface (context_cmds.go) ---

// GetLastOutput returns the text of the last assistant message.
func (a *LoopAdapter) GetLastOutput() string {
	if a.loop == nil || a.loop.Session == nil {
		return ""
	}
	msgs := a.loop.Session.Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			var parts []string
			for _, c := range msgs[i].Content {
				if c.Type == "text" {
					parts = append(parts, c.Text)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

// GetFullConversation returns the full conversation as text.
func (a *LoopAdapter) GetFullConversation() string {
	if a.loop == nil || a.loop.Session == nil {
		return ""
	}
	var sb strings.Builder
	for _, m := range a.loop.Session.Messages {
		sb.WriteString(fmt.Sprintf("[%s]\n", m.Role))
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				sb.WriteString(c.Text)
				sb.WriteString("\n")
			case "tool_use":
				fmt.Fprintf(&sb, "Tool: %s\n", c.Name)
			case "tool_result":
				fmt.Fprintf(&sb, "Result: %s\n", c.Text)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// --- LSP-dependent methods — all return ErrNotConnected ---

// errLSP returns a LoopError indicating the LSP client is not connected.
func errLSP(command string) *LoopError {
	return &LoopError{
		Kind:      LoopErrNotConnected,
		Subsystem: "lsp",
		Message:   fmt.Sprintf("LSP client not connected; /%s requires a running language server", command),
	}
}

// ListSymbols returns symbols in a file. Requires LSP client (not yet implemented).
func (a *LoopAdapter) ListSymbols(path string) ([]string, error) {
	return nil, errLSP("symbols")
}

// FindReferences finds all references to a symbol. Requires LSP client.
func (a *LoopAdapter) FindReferences(symbol string) ([]string, error) {
	return nil, errLSP("references")
}

// FindDefinition finds the definition of a symbol. Requires LSP client.
func (a *LoopAdapter) FindDefinition(symbol string) (string, error) {
	return "", errLSP("definition")
}

// GetHoverInfo returns hover information for a symbol. Requires LSP client.
func (a *LoopAdapter) GetHoverInfo(symbol string) (string, error) {
	return "", errLSP("hover")
}

// GetDiagnostics returns LSP diagnostics for a file. Requires LSP client.
func (a *LoopAdapter) GetDiagnostics(path string) ([]string, error) {
	return nil, errLSP("diagnostics")
}

// --- codeMapper interface (context_cmds.go) ---

// ShowCodeMap returns a directory tree map of the workspace.
func (a *LoopAdapter) ShowCodeMap(depth int) (string, error) {
	if a.loop == nil {
		return "", ErrSubsystemUnavailable
	}
	cmd := exec.Command("find", ".",
		"-maxdepth", fmt.Sprintf("%d", depth),
		"-type", "f",
		"(", "-name", "*.go", "-o", "-name", "*.ts", "-o", "-name", "*.py", "-o", "-name", "*.rs", ")",
	)
	cmd.Dir = a.loop.workspaceRoot()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("code map: %w", err)
	}
	if len(out) == 0 {
		return "No source files found.", nil
	}
	// Limit output to avoid flooding the terminal.
	lines := strings.SplitN(string(out), "\n", 101)
	if len(lines) > 100 {
		lines = append(lines[:100], fmt.Sprintf("... and more (limited to 100 files)"))
	}
	return strings.Join(lines, "\n"), nil
}

// --- toolDetailer interface (context_cmds.go) ---

// GetToolDetails returns detailed information about a specific tool.
func (a *LoopAdapter) GetToolDetails(name string) (string, error) {
	if a.loop == nil {
		return "", ErrSubsystemUnavailable
	}
	for _, t := range a.loop.Tools {
		if t.Name == name {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("Tool: %s\n", t.Name))
			sb.WriteString(fmt.Sprintf("Description: %s\n", t.Description))
			if t.InputSchema.Type != "" {
				sb.WriteString("Input schema: (defined)\n")
			}
			return sb.String(), nil
		}
	}
	// Check MCP tools.
	if a.hasMCP {
		for _, t := range a.loop.MCPRegistry.AllAPITools() {
			if t.Name == name {
				return fmt.Sprintf("Tool: %s (MCP)\nDescription: %s\n", t.Name, t.Description), nil
			}
		}
	}
	return "", fmt.Errorf("tool %q not found", name)
}
