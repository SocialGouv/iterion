package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/llmtypes"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ToolInfo is the MCP tool description returned by tools/list.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolCallResult is the MCP result returned by tools/call.
type ToolCallResult struct {
	Content           []ToolContent `json:"content,omitempty"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// ToolContent is one MCP content item.
type ToolContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// ResourceInfo describes a resource exposed by an MCP server.
type ResourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceContent is the body returned by resources/read.
type ResourceContent struct {
	URI         string
	Name        string
	Description string
	MimeType    string
	Text        string
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type protocolClient interface {
	Ping(ctx context.Context) error
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, toolName string, args map[string]any) (*ToolCallResult, error)
	ListResources(ctx context.Context) ([]ResourceInfo, error)
	ReadResource(ctx context.Context, uri string) (ResourceContent, error)
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
// cached for the lifetime of the manager.
//
// Transactional: if ANY configured server fails to discover, every
// server registered earlier in this call is unregistered before the
// combined error is returned. Without this, a workflow that declares
// {github, slack} and whose slack server is misconfigured would run
// with only github tools live — silently producing "unknown tool"
// errors on the first slack-bound LLM call instead of surfacing the
// misconfiguration up-front.
func (m *Manager) EnsureServers(ctx context.Context, registry *tool.Registry, servers []string) error {
	var errs []error
	var landed []string
	for _, server := range servers {
		newly, err := m.ensureServer(ctx, registry, server)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// Only roll back servers THIS call discovered — a server already
		// live from a previous EnsureServers must survive an unrelated
		// failure here, else its tools vanish from the registry.
		if newly {
			landed = append(landed, server)
		}
	}
	if len(errs) > 0 {
		for _, server := range landed {
			registry.UnregisterServer(server)
			if state, err := m.state(server); err == nil {
				state.mu.Lock()
				state.discovered = false
				state.mu.Unlock()
			}
		}
		return errors.Join(errs...)
	}
	// Persist fingerprints once after all servers are discovered.
	if m.fingerprints != nil {
		_ = m.fingerprints.Save()
	}
	return nil
}

// HealthCheck verifies that each listed server is reachable by connecting and
// sending a liveness probe (an MCP ping, or tools/list on protocol revisions
// that removed ping — see sdkClient.Ping). Connections are cached, so a
// subsequent EnsureServers call reuses the same client (no double-spawn for
// stdio servers).
//
// Pings run in parallel — sequential dispatch with a global ctx
// deadline would let an early slow server consume the whole budget
// and leave the tail un-checked while still appearing to pass.
//
// Each Ping gets its own per-server ctx timeout: a misbehaving
// stdio MCP server whose Ping ignores ctx cancellation would
// otherwise leak the goroutine for the lifetime of the daemon
// even after HealthCheck returns. The timeout bounds that leak.
func (m *Manager) HealthCheck(ctx context.Context, servers []string) error {
	if len(servers) == 0 {
		return nil
	}
	type result struct {
		server string
		err    error
	}
	results := make(chan result, len(servers))
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Add(1)
		go func(server string) {
			defer wg.Done()
			state, err := m.state(server)
			if err != nil {
				results <- result{server: server, err: fmt.Errorf("mcp: health-check %q: %w", server, err)}
				return
			}
			state.mu.Lock()
			client, err := m.clientForState(state)
			state.mu.Unlock()
			if err != nil {
				results <- result{server: server, err: fmt.Errorf("mcp: health-check %q: connect failed: %w", server, err)}
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, mcpHealthPingTimeout)
			pingErr := client.Ping(pingCtx)
			cancel()
			if pingErr != nil {
				results <- result{server: server, err: fmt.Errorf("mcp: health-check %q: ping failed: %w", server, pingErr)}
				return
			}
			results <- result{server: server}
		}(server)
	}
	wg.Wait()
	close(results)
	var errs []error
	for r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	return errors.Join(errs...)
}

// mcpHealthPingTimeout bounds the per-server Ping in HealthCheck. An
// MCP stdio child whose Ping implementation ignores the parent ctx
// would otherwise leak its health-check goroutine until process
// exit; the per-call timeout caps the leak window.
const mcpHealthPingTimeout = 5 * time.Second

// mcpDiscoveryEmptyRetries / mcpDiscoveryEmptyBackoff bound the retry-on-empty
// in ensureServer: an MCP server can answer tools/list before it finishes
// registering its tools right after initialize. Up to 6 extra re-lists spaced
// 500ms apart (~3s) covers that startup race without stalling a genuinely
// tool-less server for long.
const (
	mcpDiscoveryEmptyRetries = 6
	mcpDiscoveryEmptyBackoff = 500 * time.Millisecond
)

// ServerNames returns the names of every server known to the catalog
// (whether or not it has been connected yet). The order is stable but
// unspecified.
func (m *Manager) ServerNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.catalog))
	for name := range m.catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ServerConfig returns a clone of the catalog entry for `name`. The
// boolean is false when the server is unknown.
func (m *Manager) ServerConfig(name string) (*ServerConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.catalog[name]
	if !ok {
		return nil, false
	}
	return cloneServerConfig(cfg), true
}

// ListResources connects (lazily) to `server` and returns the
// resources it exposes. The connection is shared with subsequent
// CallTool / ReadResource calls.
func (m *Manager) ListResources(ctx context.Context, server string) ([]ResourceInfo, error) {
	state, err := m.state(server)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	client, err := m.clientForState(state)
	state.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("mcp: list resources from %q: %w", server, err)
	}
	return client.ListResources(ctx)
}

// ReadResource connects (lazily) to `server` and returns the body of
// the resource at `uri`.
func (m *Manager) ReadResource(ctx context.Context, server, uri string) (ResourceContent, error) {
	state, err := m.state(server)
	if err != nil {
		return ResourceContent{}, err
	}
	state.mu.Lock()
	client, err := m.clientForState(state)
	state.mu.Unlock()
	if err != nil {
		return ResourceContent{}, fmt.Errorf("mcp: read resource from %q: %w", server, err)
	}
	return client.ReadResource(ctx, uri)
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

// ensureServer discovers and registers one server's tools. newlyDiscovered is
// true only when THIS call performed the discovery, so the caller's
// transactional rollback never tears down a server that was already live from
// a previous call.
func (m *Manager) ensureServer(ctx context.Context, registry *tool.Registry, server string) (newlyDiscovered bool, err error) {
	state, err := m.state(server)
	if err != nil {
		return false, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.discovered {
		return false, nil
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
		return false, err
	}

	didListTools := false
	if toolsList == nil {
		didListTools = true
		var listErr error
		// Retry-on-empty. A server (notably fastmcp-based ones like
		// firecrawl-mcp) can answer tools/list right after `initialize` but
		// BEFORE it finishes registering its tools, returning [] on the first
		// call. Re-list a few times with a short backoff on the SAME session so
		// a startup race doesn't leave the server tool-less for the whole run.
		// A genuinely tool-less server just costs a few cheap re-lists.
		for attempt := 0; ; attempt++ {
			toolsList, listErr = client.ListTools(ctx)
			if listErr != nil {
				return false, fmt.Errorf("mcp: discover tools for %q: %w", server, listErr)
			}
			if len(toolsList) > 0 || attempt >= mcpDiscoveryEmptyRetries {
				break
			}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(mcpDiscoveryEmptyBackoff):
			}
		}
		// NEVER cache an empty tool list — a startup race (above) shouldn't
		// poison the persistent disk cache and starve EVERY later run on the pod
		// (fast empty cache hit → 0 tools forever). Only a non-empty discovery
		// is durable enough to cache.
		if m.cache != nil && len(toolsList) > 0 {
			_ = m.cache.Set(server, state.cfg, toolsList) // best-effort
		}
	}

	workDir := state.cfg.WorkDir
	for _, info := range toolsList {
		serverName := server
		toolName := info.Name
		// Pre-compute whether this tool actually declares a `limit`
		// integer property in its input schema. The auto-retry below
		// only fires when both the tool name is "Read" AND the schema
		// shape matches the Claude Code Read tool. A third-party MCP
		// server that exposes a tool literally named "Read" with a
		// different signature won't have its args overwritten.
		toolHasLimitParam := schemaDeclaresLimit(info.InputSchema)
		if err := registry.RegisterMCP(serverName, toolName, info.Description, info.InputSchema, func(callCtx context.Context, input json.RawMessage) (string, error) {
			var args map[string]any
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
			// Gated on (a) tool name, (b) error string, (c) schema declares limit,
			// (d) caller didn't already set a limit (don't clobber).
			if fmtErr != nil && toolName == "Read" && toolHasLimitParam &&
				strings.Contains(fmtErr.Error(), "exceeds maximum allowed tokens") {
				if args == nil {
					args = make(map[string]any)
				}
				// Shrink to 300 and retry when no caller-supplied limit exists
				// OR the current limit is still above 300. The DefaultSanitization
				// rules inject limit=500, so a bare `!callerSet` guard never fired —
				// the retry path was effectively dead. The float64 assertion is
				// guarded so a non-numeric limit falls through to the shrink.
				curLimit, isFloat := args["limit"].(float64)
				if _, callerSet := args["limit"]; !callerSet || !isFloat || curLimit > 300 {
					args["limit"] = float64(300)
					retryResult, retryErr := client.CallTool(callCtx, toolName, args)
					if retryErr != nil {
						return "", retryErr
					}
					return formatToolResult(retryResult)
				}
			}
			return text, fmtErr
		}); err != nil {
			return false, fmt.Errorf("mcp: register %s.%s: %w", server, info.Name, err)
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

	return true, nil
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
		args := map[string]any{
			"command":     "ls >/dev/null 2>&1 && pwd",
			"description": "smoke test: verify workspace access",
		}
		result, err := client.CallTool(ctx, "Bash", args)
		if err != nil {
			return fmt.Errorf("bash tool call failed: %w", err)
		}
		text, _ := formatToolResult(result)
		if !strings.Contains(text, workDir) {
			return fmt.Errorf("bash pwd returned %q, expected workDir %q", strings.TrimSpace(text), workDir)
		}
	case toolNames["codex"]:
		// Codex exposes a single "codex" tool — ask it to list files.
		args := map[string]any{
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
	return strutil.ContainsAnyFold(msg, fatalPatterns)
}

// schemaDeclaresLimit reports whether an MCP tool's JSON-schema input
// declares a `limit` property (any type). Used to gate the Read
// auto-retry: we only mutate args[limit] when the tool actually
// accepts that parameter, so a third-party server exposing an
// unrelated tool literally named "Read" stays untouched.
func schemaDeclaresLimit(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	_, ok := s.Properties["limit"]
	return ok
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
	return strings.Join(out, "\n")
}

func (m *Manager) applySanitizationRules(toolName string, args map[string]any, workDir string) {
	if args == nil {
		return
	}
	for _, rule := range m.sanitizationRules {
		if rule.Match(toolName) {
			rule.Apply(toolName, args, workDir)
		}
	}
}
