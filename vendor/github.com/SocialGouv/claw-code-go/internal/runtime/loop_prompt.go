package runtime

import (
	"context"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os/exec"
	"strings"
	"time"
)

// --- promptInjector interface (code_cmds.go) ---

// InjectPrompt appends a message to the session with IsInjected metadata.
// This flag prevents compaction from counting injected messages as real user turns
// and prevents token accounting confusion.
func (a *LoopAdapter) InjectPrompt(prompt string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if a.loop.Session == nil {
		return NewLoopError(LoopErrSubsystemUnavailable, "session", "no active session")
	}
	a.loop.Session.Messages = append(a.loop.Session.Messages, api.Message{
		Role: "user",
		Content: []api.ContentBlock{
			{Type: "text", Text: prompt},
		},
		IsInjected: true,
	})
	// Track in prompt history with injection marker.
	a.loop.Session.PromptHistory = append(a.loop.Session.PromptHistory, PromptHistoryEntry{
		TimestampMs: time.Now().UnixMilli(),
		Text:        "[injected] " + truncate(prompt, 100),
	})
	return nil
}

// --- commandRunner interface ---

// RunCommand executes a shell command and returns its output.
func (a *LoopAdapter) RunCommand(command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	if a.loop != nil {
		cmd.Dir = a.loop.workspaceRoot()
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("command failed: %w\n%s", err, string(out))
	}
	return string(out), nil
}

// --- chatMode toggle ---

// ChatMode returns whether free-form chat mode is active.
func (a *LoopAdapter) ChatMode() bool {
	return a.chatMode
}

// SetChatMode enables or disables free-form chat mode.
func (a *LoopAdapter) SetChatMode(on bool) {
	a.chatMode = on
}

// --- webFetcher interface ---

// WebFetch fetches the content of a URL. This delegates to the web_fetch tool
// when available; otherwise returns an error.
func (a *LoopAdapter) WebFetch(url string) (string, error) {
	if err := a.requireLoop(); err != nil {
		return "", err
	}
	// Delegate to the web_fetch tool via ExecuteTool.
	result := a.loop.ExecuteTool(context.Background(), "web_fetch", map[string]any{
		"url": url,
	})
	if result.IsError {
		return "", fmt.Errorf("web_fetch: %s", result.Text)
	}
	return result.Text, nil
}

// --- effortLoop interface (ux_cmds.go) ---

// SetEffort sets the effort level (low, medium, high).
func (a *LoopAdapter) SetEffort(level string) error {
	level = strings.ToLower(strings.TrimSpace(level))
	switch level {
	case "low", "medium", "high":
		if a.loop != nil && a.loop.Config != nil {
			a.loop.Config.ReasoningEffort = level
		}
		return nil
	default:
		return NewLoopError(LoopErrInvalidArgs, "config", fmt.Sprintf("unknown effort level %q (use low, medium, high)", level))
	}
}

// GetEffort returns the current effort level.
func (a *LoopAdapter) GetEffort() string {
	if a.loop != nil && a.loop.Config != nil && a.loop.Config.ReasoningEffort != "" {
		return a.loop.Config.ReasoningEffort
	}
	return "medium"
}

// --- themeLoop interface (ux_cmds.go) ---

// SetTheme sets the color theme.
func (a *LoopAdapter) SetTheme(name string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	a.loop.Config.Theme = name
	return nil
}

// CurrentTheme returns the current theme name.
func (a *LoopAdapter) CurrentTheme() string {
	if a.loop != nil && a.loop.Config != nil && a.loop.Config.Theme != "" {
		return a.loop.Config.Theme
	}
	return "dark"
}

// ListThemes returns available theme names.
func (a *LoopAdapter) ListThemes() []string {
	return []string{"dark", "light", "monokai", "solarized"}
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
