package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/llmtypes"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	iterlog "github.com/SocialGouv/iterion/pkg/internal/log"
)

// DefaultProtocolVersion is the MCP version advertised by Iterion.
const DefaultProtocolVersion = "2025-06-18"

// ToolInfo is the MCP tool description returned by tools/list.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolCallResult is the MCP result returned by tools/call.
type ToolCallResult struct {
	Content           []ToolContent `json:"content,omitempty"`
	StructuredContent interface{}   `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// ToolContent is one MCP content item.
type ToolContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type protocolClient interface {
	Ping(ctx context.Context) error
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*ToolCallResult, error)
	Close() error
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithSanitizationRules replaces the default sanitization rules.
func WithSanitizationRules(rules []SanitizationRule) ManagerOption {
	return func(m *Manager) { m.sanitizationRules = rules }
}

// WithLogger sets a leveled logger on the manager.
func WithLogger(l *iterlog.Logger) ManagerOption {
	return func(m *Manager) { m.logger = l }
}

// Manager lazily connects to MCP servers, caches clients and tool discovery,
// and bridges discovered tools into a tool.Registry.
type Manager struct {
	mu                sync.Mutex
	catalog           map[string]*ServerConfig
	states            map[string]*serverState
	sanitizationRules []SanitizationRule
	cache             *ToolCache
	fingerprints      *FingerprintStore
	logger            *iterlog.Logger
}

type serverState struct {
	cfg *ServerConfig

	mu         sync.Mutex
	client     protocolClient
	discovered bool
}

// NewManager creates an MCP manager from a resolved server catalog.
func NewManager(catalog map[string]*ServerConfig, opts ...ManagerOption) *Manager {
	cloned := make(map[string]*ServerConfig, len(catalog))
	for name, cfg := range catalog {
		cloned[name] = cloneServerConfig(cfg)
	}
	m := &Manager{
		catalog:           cloned,
		states:            make(map[string]*serverState, len(cloned)),
		sanitizationRules: DefaultSanitizationRules(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// EnsureServers discovers tools for the given servers and registers them into
// the provided registry. Connections and tool catalogs are opened lazily and
// cached for the lifetime of the manager. If some servers fail, discovery
// continues for the remaining servers and a combined error is returned.
func (m *Manager) EnsureServers(ctx context.Context, registry *tool.Registry, servers []string) error {
	var errs []error
	for _, server := range servers {
		if err := m.ensureServer(ctx, registry, server); err != nil {
			errs = append(errs, err)
		}
	}
	// Persist fingerprints once after all servers are discovered.
	if m.fingerprints != nil {
		_ = m.fingerprints.Save()
	}
	return errors.Join(errs...)
}

// HealthCheck verifies that each listed server is reachable by connecting and
// sending an MCP ping. Connections are cached, so a subsequent EnsureServers
// call reuses the same client (no double-spawn for stdio servers).
func (m *Manager) HealthCheck(ctx context.Context, servers []string) error {
	var errs []error
	for _, server := range servers {
		state, err := m.state(server)
		if err != nil {
			errs = append(errs, fmt.Errorf("mcp: health-check %q: %w", server, err))
			continue
		}
		state.mu.Lock()
		client, err := m.clientForState(state)
		if err != nil {
			state.mu.Unlock()
			errs = append(errs, fmt.Errorf("mcp: health-check %q: connect failed: %w", server, err))
			continue
		}
		state.mu.Unlock()
		if err := client.Ping(ctx); err != nil {
			errs = append(errs, fmt.Errorf("mcp: health-check %q: ping failed: %w", server, err))
		}
	}
	return errors.Join(errs...)
}

// Close closes any open MCP clients held by the manager.
func (m *Manager) Close() error {
	m.mu.Lock()
	states := make([]*serverState, 0, len(m.states))
	for _, state := range m.states {
		states = append(states, state)
	}
	m.mu.Unlock()

	var firstErr error
	for _, state := range states {
		state.mu.Lock()
		client := state.client
		state.mu.Unlock()
		if client == nil {
			continue
		}
		if err := client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Manager) ensureServer(ctx context.Context, registry *tool.Registry, server string) error {
	state, err := m.state(server)
	if err != nil {
		return err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.discovered {
		return nil
	}

	// Try cache first to avoid the ListTools RPC latency.
	var toolsList []ToolInfo
	if m.cache != nil {
		if cached, ok := m.cache.Get(server, state.cfg); ok {
			toolsList = cached
		}
	}

	// Establish a client connection. Even with a cache hit we need the
	// client for CallTool; the connection is lazy (deferred to first call).
	client, err := m.clientForState(state)
	if err != nil {
		return err
	}

	didListTools := false
	if toolsList == nil {
		didListTools = true
		var listErr error
		toolsList, listErr = client.ListTools(ctx)
		if listErr != nil {
			return fmt.Errorf("mcp: discover tools for %q: %w", server, listErr)
		}
		if m.cache != nil {
			_ = m.cache.Set(server, state.cfg, toolsList) // best-effort
		}
	}

	workDir := state.cfg.WorkDir
	for _, info := range toolsList {
		serverName := server
		toolName := info.Name
		if err := registry.RegisterMCP(serverName, toolName, info.Description, info.InputSchema, func(callCtx context.Context, input json.RawMessage) (string, error) {
			var args map[string]interface{}
			if len(input) > 0 && string(input) != "null" {
				if err := json.Unmarshal(input, &args); err != nil {
					return "", fmt.Errorf("mcp: decode input for %s.%s: %w", serverName, toolName, err)
				}
			}
			m.applySanitizationRules(toolName, args, workDir)
			result, err := client.CallTool(callCtx, toolName, args)
			if err != nil {
				return "", err
			}
			text, fmtErr := formatToolResult(result)
			// Auto-retry Read with a smaller limit on "exceeds maximum tokens".
			if fmtErr != nil && toolName == "Read" && strings.Contains(fmtErr.Error(), "exceeds maximum allowed tokens") {
				args["limit"] = float64(300)
				retryResult, retryErr := client.CallTool(callCtx, toolName, args)
				if retryErr != nil {
					return "", retryErr
				}
				return formatToolResult(retryResult)
			}
			return text, fmtErr
		}); err != nil {
			return fmt.Errorf("mcp: register %s.%s: %w", server, info.Name, err)
		}
		if m.fingerprints != nil {
			qualified := "mcp." + serverName + "." + toolName
			if change := m.fingerprints.Check(qualified, serverName, toolName, info.InputSchema); change != nil {
				if !change.IsNew {
					m.logger.Warn("schema changed for %q (was %s, now %s)",
						change.QualifiedName, change.PreviousFingerprint[:12], change.CurrentFingerprint[:12])
				}
			}
		}
	}

	state.discovered = true

	// Smoke-test workspace access on live discovery only (skip on cache hit).
	if workDir != "" && didListTools {
		if err := m.smokeTestWorkspace(ctx, client, server, toolsList, workDir); err != nil {
			// Log as warning — don't block server discovery, but make it visible.
			m.logger.Warn("workspace smoke test failed for %q (workDir=%s): %v", server, workDir, err)
		}
	}

	return nil
}

// smokeTestWorkspace verifies that an MCP server can access the configured
// workspace directory by calling a lightweight tool (Bash "pwd", Read, or
// the codex tool with a trivial prompt). This catches workspace access
// problems at startup rather than mid-workflow.
func (m *Manager) smokeTestWorkspace(ctx context.Context, client protocolClient, server string, tools []ToolInfo, workDir string) error {
	// Strategy: pick the simplest available tool to verify filesystem access.
	toolNames := make(map[string]bool, len(tools))
	for _, t := range tools {
		toolNames[t.Name] = true
	}

	switch {
	case toolNames["Bash"]:
		// Claude Code exposes Bash — run a quick "ls" in workDir.
		args := map[string]interface{}{
			"command":     "ls >/dev/null 2>&1 && pwd",
			"description": "smoke test: verify workspace access",
		}
		result, err := client.CallTool(ctx, "Bash", args)
		if err != nil {
			return fmt.Errorf("Bash tool call failed: %w", err)
		}
		text, _ := formatToolResult(result)
		if !strings.Contains(text, workDir) {
			return fmt.Errorf("Bash pwd returned %q, expected workDir %q", strings.TrimSpace(text), workDir)
		}
	case toolNames["codex"]:
		// Codex exposes a single "codex" tool — ask it to list files.
		args := map[string]interface{}{
			"prompt":          "Run: ls -la . && pwd",
			"cwd":             workDir,
			"sandbox":         "read-only",
			"approval-policy": "never",
		}
		result, err := client.CallTool(ctx, "codex", args)
		if err != nil {
			return fmt.Errorf("codex tool call failed: %w", err)
		}
		text, _ := formatToolResult(result)
		if strings.Contains(text, "No such file") || strings.Contains(text, "not found") {
			return fmt.Errorf("codex cannot access workDir %q: %s", workDir, text)
		}
	default:
		// No suitable tool for smoke test — skip silently.
		return nil
	}
	return nil
}

func (m *Manager) state(server string) (*serverState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, ok := m.states[server]; ok {
		return state, nil
	}
	cfg, ok := m.catalog[server]
	if !ok {
		return nil, fmt.Errorf("mcp: unknown server %q", server)
	}
	state := &serverState{cfg: cloneServerConfig(cfg)}
	m.states[server] = state
	return state, nil
}

func (m *Manager) clientForState(state *serverState) (protocolClient, error) {
	if state.client != nil {
		return state.client, nil
	}

	info := clientInfo{
		Name:    appinfo.Name,
		Version: appinfo.FullVersion(),
	}

	state.client = newSDKClient(state.cfg, info)
	return state.client, nil
}

// FatalToolError wraps MCP tool errors that should not be retried or
// absorbed by the LLM tool loop (e.g. rate limits, credit exhaustion).
// Implements llmtypes.FatalToolError so the generation loop stops.
type FatalToolError struct {
	Message string
}

var _ llmtypes.FatalToolError = (*FatalToolError)(nil)

func (e *FatalToolError) Error() string {
	return e.Message
}

// IsFatal implements llmtypes.FatalToolError.
func (e *FatalToolError) IsFatal() bool {
	return true
}

// fatalPatterns are substrings that indicate an MCP tool error is fatal and
// should stop the node rather than being passed back to the model.
var fatalPatterns = []string{
	"usage_limit",
	"rate_limit",
	"rate limit",
	"hit your usage limit",
	"quota exceeded",
	"credit",
	"billing",
	"authentication",
	"unauthorized",
	"forbidden",
}

func isFatalMCPError(msg string) bool {
	lower := strings.ToLower(msg)
	for _, pattern := range fatalPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func formatToolResult(result *ToolCallResult) (string, error) {
	if result == nil {
		return "", nil
	}
	if result.IsError {
		msg := stringsFromContent(result.Content)
		if msg == "" && result.StructuredContent != nil {
			data, _ := json.Marshal(result.StructuredContent)
			msg = string(data)
		}
		if msg == "" {
			msg = "tool returned an MCP error"
		}
		// Check for fatal errors that should stop the node immediately.
		if isFatalMCPError(msg) {
			return "", &FatalToolError{Message: fmt.Sprintf("mcp: FATAL: %s", msg)}
		}
		return "", fmt.Errorf("mcp: %s", msg)
	}
	if result.StructuredContent != nil && len(result.Content) == 0 {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("mcp: marshal structured result: %w", err)
		}
		return string(data), nil
	}
	text := stringsFromContent(result.Content)
	if text != "" {
		return text, nil
	}
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("mcp: marshal structured result: %w", err)
		}
		return string(data), nil
	}
	return "", nil
}

func stringsFromContent(content []ToolContent) string {
	out := make([]string, 0, len(content))
	for _, item := range content {
		if item.Type == "" || item.Type == "text" {
			if item.Text != "" {
				out = append(out, item.Text)
			}
		}
	}
	return joinLines(out)
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}

func (m *Manager) applySanitizationRules(toolName string, args map[string]interface{}, workDir string) {
	if args == nil {
		return
	}
	for _, rule := range m.sanitizationRules {
		if rule.Match(toolName) {
			rule.Apply(toolName, args, workDir)
		}
	}
}
