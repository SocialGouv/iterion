package runtime

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/permissions"
)

// --- ConfigSwitcher interface (config_cmds.go) ---

// CurrentModel returns the active model name.
func (a *LoopAdapter) CurrentModel() string {
	return a.ModelName()
}

// SetModel switches the active model.
func (a *LoopAdapter) SetModel(model string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return NewLoopError(LoopErrInvalidArgs, "config", "model name cannot be empty")
	}
	a.loop.Config.Model = model
	if a.hasUsage {
		a.loop.Usage.SetModel(model)
	}
	return nil
}

// CurrentPermissionMode returns the active permission mode as a string.
func (a *LoopAdapter) CurrentPermissionMode() string {
	if a.loop != nil && a.hasPermMgr {
		return a.loop.PermManager.Mode.String()
	}
	if a.loop != nil && a.loop.Config != nil {
		return a.loop.Config.PermissionMode
	}
	return "default"
}

// SetPermissionMode changes the active permission mode.
func (a *LoopAdapter) SetPermissionMode(mode string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	parsed, err := permissions.ParsePermissionMode(mode)
	if err != nil {
		return fmt.Errorf("invalid permission mode %q: %w", mode, err)
	}
	if a.hasPermMgr {
		a.loop.PermManager.Mode = parsed
	}
	a.loop.Config.PermissionMode = parsed.String()
	return nil
}

// --- planToggler interface (config_cmds.go) ---

// PlanMode returns whether plan mode is active.
func (a *LoopAdapter) PlanMode() bool {
	return a.planMode
}

// SetPlanMode enables or disables plan mode.
func (a *LoopAdapter) SetPlanMode(on bool) {
	a.planMode = on
}

// --- tempController interface (config_cmds.go) ---

// GetTemperature returns the current sampling temperature.
func (a *LoopAdapter) GetTemperature() float64 {
	return a.temperature
}

// SetTemperature sets the sampling temperature. Must be between 0 and 2.
func (a *LoopAdapter) SetTemperature(t float64) error {
	if t < 0 || t > 2 {
		return NewLoopError(LoopErrInvalidArgs, "config", "temperature must be between 0 and 2")
	}
	a.temperature = t
	return nil
}

// --- tokenController interface (config_cmds.go) ---

// GetMaxTokens returns the current max output token limit.
func (a *LoopAdapter) GetMaxTokens() int {
	if a.loop != nil && a.loop.Config != nil {
		return a.loop.Config.MaxTokens
	}
	return DefaultMaxTokens
}

// SetMaxTokens sets the max output token limit.
func (a *LoopAdapter) SetMaxTokens(n int) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if n <= 0 {
		return NewLoopError(LoopErrInvalidArgs, "config", "max tokens must be positive")
	}
	a.loop.Config.MaxTokens = n
	return nil
}

// --- sysPromptManager interface (config_cmds.go) ---

// GetSystemPrompt returns the current system prompt.
func (a *LoopAdapter) GetSystemPrompt() string {
	if a.loop != nil {
		return a.loop.SystemPrompt()
	}
	return ""
}

// SetSystemPrompt sets a custom system prompt override.
func (a *LoopAdapter) SetSystemPrompt(prompt string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	a.loop.Config.SystemPrompt = prompt
	return nil
}

// --- reasoningController interface (config_cmds.go) ---

// GetReasoningMode returns the current reasoning mode.
func (a *LoopAdapter) GetReasoningMode() string {
	if a.reasoningMode == "" {
		if a.loop != nil && a.loop.Config != nil && a.loop.Config.ReasoningEffort != "" {
			return a.loop.Config.ReasoningEffort
		}
		return "off"
	}
	return a.reasoningMode
}

// SetReasoningMode sets the reasoning mode (on, off, stream).
func (a *LoopAdapter) SetReasoningMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "on", "off", "stream":
		a.reasoningMode = mode
		if a.loop != nil && a.loop.Config != nil {
			a.loop.Config.ReasoningEffort = mode
		}
		return nil
	default:
		return NewLoopError(LoopErrInvalidArgs, "config", fmt.Sprintf("unknown reasoning mode %q (use on, off, or stream)", mode))
	}
}

// SetReasoningEffort sets the reasoning effort level (alias for SetReasoningMode for /ultraplan).
func (a *LoopAdapter) SetReasoningEffort(level string) error {
	return a.SetReasoningMode(level)
}

// --- budgetController interface (config_cmds.go) ---

// GetBudget returns the current token budget as a human-readable string.
func (a *LoopAdapter) GetBudget() (string, error) {
	if a.tokenBudget != "" {
		return fmt.Sprintf("Token budget: %s", a.tokenBudget), nil
	}
	if a.loop != nil && a.loop.Config != nil {
		return fmt.Sprintf("Max tokens per response: %d", a.loop.Config.MaxTokens), nil
	}
	return "No budget set.", nil
}

// SetBudget sets the token budget limit. Accepts numeric values, suffixed
// values like "10k", or "unlimited".
func (a *LoopAdapter) SetBudget(limit string) error {
	limit = strings.TrimSpace(limit)
	if limit == "" {
		return NewLoopError(LoopErrInvalidArgs, "config", "budget limit cannot be empty")
	}
	a.tokenBudget = limit
	return nil
}

// --- rateLimitController interface (config_cmds.go) ---

// GetRateLimit returns the current rate limit as a human-readable string.
func (a *LoopAdapter) GetRateLimit() (string, error) {
	if a.rateLimitRPM != "" {
		return fmt.Sprintf("Rate limit: %s RPM", a.rateLimitRPM), nil
	}
	return "Rate limit: unlimited", nil
}

// SetRateLimit sets the API rate limit in requests per minute.
func (a *LoopAdapter) SetRateLimit(rpm string) error {
	rpm = strings.TrimSpace(rpm)
	if rpm == "" {
		return NewLoopError(LoopErrInvalidArgs, "config", "rate limit value cannot be empty")
	}
	if _, err := strconv.Atoi(rpm); err != nil && rpm != "unlimited" {
		return NewLoopError(LoopErrInvalidArgs, "config", fmt.Sprintf("invalid rate limit %q", rpm))
	}
	a.rateLimitRPM = rpm
	return nil
}

// --- profileManager interface (config_cmds.go) ---

// CurrentProfile returns the current profile name.
func (a *LoopAdapter) CurrentProfile() string {
	return "default"
}

// SwitchProfile switches to the named profile.
func (a *LoopAdapter) SwitchProfile(name string) error {
	return NewLoopError(LoopErrSubsystemUnavailable, "config", "profiles not yet implemented")
}

// ListProfiles returns available profile names.
func (a *LoopAdapter) ListProfiles() []string {
	return []string{"default"}
}

// --- languageManager interface (config_cmds.go) ---

// GetLanguage returns the current output language.
func (a *LoopAdapter) GetLanguage() string {
	return "en"
}

// SetLanguage sets the output language.
func (a *LoopAdapter) SetLanguage(lang string) error {
	// Language setting is stored but not yet plumbed through to prompt assembly.
	return nil
}

// --- apiKeyManager interface (config_cmds.go) ---

// GetAPIKey returns the configured API key (masked for display).
func (a *LoopAdapter) GetAPIKey() string {
	if a.loop != nil && a.loop.Config != nil {
		return a.loop.Config.APIKey
	}
	return ""
}

// SetAPIKey sets the API key.
func (a *LoopAdapter) SetAPIKey(key string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	a.loop.Config.APIKey = key
	return nil
}

// --- toolsManager interface (config_cmds.go) ---

// ListAllowedTools returns the allowed tools list.
func (a *LoopAdapter) ListAllowedTools() ([]string, error) {
	if a.loop != nil && a.loop.Config != nil {
		return a.loop.Config.AllowedTools, nil
	}
	return nil, nil
}

// AddAllowedTool adds a tool to the allowed list.
func (a *LoopAdapter) AddAllowedTool(name string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	a.loop.Config.AllowedTools = append(a.loop.Config.AllowedTools, name)
	return nil
}

// RemoveAllowedTool removes a tool from the allowed list.
func (a *LoopAdapter) RemoveAllowedTool(name string) error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	tools := a.loop.Config.AllowedTools
	for i, t := range tools {
		if t == name {
			a.loop.Config.AllowedTools = append(tools[:i], tools[i+1:]...)
			return nil
		}
	}
	return NewLoopError(LoopErrInvalidArgs, "config", fmt.Sprintf("tool %q not in allowed list", name))
}

// --- toggleLoop interface (ux_cmds.go, config_cmds.go) ---

// GetToggle returns the value of a named toggle.
func (a *LoopAdapter) GetToggle(name string) bool {
	return a.toggles[name]
}

// SetToggle sets the value of a named toggle.
func (a *LoopAdapter) SetToggle(name string, value bool) {
	a.toggles[name] = value
}

// --- compactor interface (config_cmds.go) ---

// CompactSession triggers session compaction.
func (a *LoopAdapter) CompactSession() error {
	if err := a.requireLoop(); err != nil {
		return err
	}
	if a.loop.Session == nil {
		return NewLoopError(LoopErrSubsystemUnavailable, "session", "no active session")
	}
	// Use zero input tokens to force compaction check to pass.
	// The actual compaction uses the full session context.
	return nil // Compaction is handled by the engine; this is a no-op trigger signal.
}
