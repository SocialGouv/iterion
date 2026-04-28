package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	agenttracer "github.com/ethpandaops/agent-sdk-observability/tracer"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/elicitation"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/observability"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/permission"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/schema"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/userinput"
)

const (
	// defaultInitializeTimeout is the default timeout for initialize control requests.
	defaultInitializeTimeout = 60 * time.Second

	// outcomeError is the tool call outcome value for failed invocations.
	outcomeError = "error"
)

// Session encapsulates protocol handling logic for MCP servers and callbacks.
type Session struct {
	log        *slog.Logger
	controller *Controller
	options    *config.Options

	sdkMcpServers   map[string]mcp.ServerInstance
	sdkDynamicTools map[string]*config.DynamicTool

	initMu               sync.RWMutex
	initializationResult map[string]any
}

// NewSession creates a new Session for protocol handling.
func NewSession(
	log *slog.Logger,
	controller *Controller,
	options *config.Options,
) *Session {
	return &Session{
		log:             log.With("component", "session"),
		controller:      controller,
		options:         options,
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}
}

// observer returns the Observer from options, or nil if unconfigured.
func (s *Session) observer() *observability.Observer {
	if s.options == nil {
		return nil
	}

	return s.options.Observer
}

// RegisterHandlers registers protocol handlers for MCP tool calls and
// command approval requests.
func (s *Session) RegisterHandlers() {
	s.controller.RegisterHandler("item_tool/call", s.HandleDynamicToolCall)
	s.controller.RegisterHandler("can_use_tool", s.HandleCanUseTool)
	s.controller.RegisterHandler("item_commandExecution/requestApproval", s.HandleCanUseTool)
	s.controller.RegisterHandler("item_commandExecution_requestApproval", s.HandleCanUseTool)
	s.controller.RegisterHandler("execCommandApproval", s.HandleCanUseTool)
	s.controller.RegisterHandler("item_tool/requestUserInput", s.HandleRequestUserInput)

	// File change approval (app-server sends item/fileChange/requestApproval).
	s.controller.RegisterHandler("item_fileChange/requestApproval", s.HandleFileChangeApproval)
	s.controller.RegisterHandler("item_fileChange_requestApproval", s.HandleFileChangeApproval)
	s.controller.RegisterHandler("applyPatchApproval", s.HandleFileChangeApproval)

	// MCP elicitation (app-server sends mcpServer/elicitation/request).
	s.controller.RegisterHandler("mcpServer_elicitation/request", s.HandleMCPElicitation)

	// Permissions approval (app-server sends item/permissions/requestApproval).
	s.controller.RegisterHandler("item_permissions/requestApproval", s.HandlePermissionsApproval)

	// External auth token refresh (used by current app-server external-auth mode).
	s.controller.RegisterHandler("account_chatgptAuthTokens/refresh", s.HandleChatGPTAuthTokensRefresh)
}

// RegisterMCPServers extracts and registers SDK MCP servers from options.
func (s *Session) RegisterMCPServers() {
	if s.options == nil || s.options.MCPServers == nil {
		return
	}

	for serverKey, serverConfig := range s.options.MCPServers {
		if serverConfig == nil {
			continue
		}

		sdkConfig, ok := serverConfig.(*mcp.SdkServerConfig)
		if !ok {
			continue
		}

		if sdkConfig.Instance == nil {
			continue
		}

		server, ok := sdkConfig.Instance.(mcp.ServerInstance)
		if !ok {
			continue
		}

		s.sdkMcpServers[serverKey] = server
		s.log.Debug("registered SDK MCP server", slog.String("server", serverKey))
	}
}

// RegisterDynamicTools indexes SDK dynamic tools by name for dispatch.
func (s *Session) RegisterDynamicTools() {
	if s.options == nil || len(s.options.SDKTools) == 0 {
		return
	}

	for _, tool := range s.options.SDKTools {
		s.sdkDynamicTools[tool.Name] = tool
		s.log.Debug("registered dynamic tool", slog.String("tool", tool.Name))
	}
}

// Initialize sends the initialization control request to the CLI.
func (s *Session) Initialize(ctx context.Context) error {
	s.log.DebugContext(ctx, "sending initialize request")

	payload := s.buildInitializePayload()

	timeout := s.getInitializeTimeout()

	resp, err := s.controller.SendRequest(ctx, "initialize", payload, timeout)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	s.initMu.Lock()
	s.initializationResult = resp.Payload()
	s.initMu.Unlock()

	return nil
}

// buildInitializePayload builds thread/start initialization payload from options.
func (s *Session) buildInitializePayload() map[string]any {
	payload := make(map[string]any, 16)

	if s.options == nil {
		return payload
	}

	if s.options.Model != "" {
		payload["model"] = s.options.Model
	}

	if s.options.MaxTurns > 0 {
		payload["maxTurns"] = s.options.MaxTurns
	}

	if s.options.Cwd != "" {
		payload["cwd"] = s.options.Cwd
	}

	if s.options.SystemPromptPreset != nil {
		payload["systemPromptPreset"] = s.options.SystemPromptPreset
	} else if s.options.SystemPrompt != "" {
		payload["systemPrompt"] = s.options.SystemPrompt
	}

	if s.options.ContinueConversation {
		payload["continueConversation"] = true
	}

	if s.options.Resume != "" {
		payload["resume"] = s.options.Resume
	}

	if s.options.ForkSession {
		payload["forkSession"] = true
	}

	if s.options.Effort != nil {
		payload["reasoningEffort"] = string(*s.options.Effort)
	}

	if s.options.Personality != "" {
		payload["personality"] = s.options.Personality
	}

	if s.options.ServiceTier != "" {
		payload["serviceTier"] = s.options.ServiceTier
	}

	if s.options.DeveloperInstructions != "" {
		payload["developerInstructions"] = s.options.DeveloperInstructions
	}

	sandboxMode := s.options.Sandbox
	if sandboxMode == "" {
		sandboxMode = mapPermissionToSandbox(s.options.PermissionMode)
	}

	if sandboxMode != "" {
		payload["sandbox"] = sandboxMode
	}

	if approvalPolicy := mapPermissionToApprovalPolicy(s.options.PermissionMode); approvalPolicy != "" {
		payload["approvalPolicy"] = approvalPolicy
	}

	if s.options.PermissionMode != "" {
		payload["permissionMode"] = s.options.PermissionMode
	}

	if len(s.options.AllowedTools) > 0 {
		payload["allowedTools"] = s.options.AllowedTools
	}

	if len(s.options.DisallowedTools) > 0 {
		payload["disallowedTools"] = s.options.DisallowedTools
	}

	switch t := s.options.Tools.(type) {
	case config.ToolsList:
		payload["tools"] = t
	case *config.ToolsPreset:
		payload["tools"] = t
	}

	if len(s.options.AddDirs) > 0 {
		payload["addDirs"] = s.options.AddDirs
	}

	if servers := serializeMCPServers(s.options.MCPServers); len(servers) > 0 {
		payload["mcpServers"] = servers
	}

	if dynamicTools := serializeDynamicTools(s.options.SDKTools); len(dynamicTools) > 0 {
		payload["dynamicTools"] = dynamicTools
	}

	if len(s.options.Config) > 0 {
		cfg := make(map[string]any, len(s.options.Config))
		for k, v := range s.options.Config {
			cfg[k] = v
		}

		payload["config"] = cfg
	}

	if s.options.OutputSchema != "" {
		payload["outputSchema"] = s.options.OutputSchema
	} else if schema := extractOutputSchema(s.options.OutputFormat); schema != nil {
		payload["outputSchema"] = schema
	}

	if s.options.PermissionPromptToolName != "" {
		payload["permissionPromptToolName"] = s.options.PermissionPromptToolName
	}

	return payload
}

// serializeDynamicTools converts SDK dynamic tools into the flat array format
// expected by the Codex CLI dynamicTools API.
func serializeDynamicTools(tools []*config.DynamicTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	result := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		entry := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
		}

		if tool.InputSchema != nil {
			entry["inputSchema"] = tool.InputSchema
		}

		result = append(result, entry)
	}

	return result
}

// serializeMCPServers converts MCP server configs into a map suitable for the
// initialize payload. SDK servers are serialized as {"type":"sdk"} so the CLI
// routes tool calls back through the control protocol.
func serializeMCPServers(servers map[string]mcp.ServerConfig) map[string]any {
	if len(servers) == 0 {
		return nil
	}

	result := make(map[string]any, len(servers))

	for name, serverCfg := range servers {
		if serverCfg == nil {
			continue
		}

		switch cfg := serverCfg.(type) {
		case *mcp.StdioServerConfig:
			entry := map[string]any{
				"type":    string(cfg.GetType()),
				"command": cfg.Command,
			}

			if len(cfg.Args) > 0 {
				entry["args"] = cfg.Args
			}

			if len(cfg.Env) > 0 {
				entry["env"] = cfg.Env
			}

			result[name] = entry
		case *mcp.SSEServerConfig:
			entry := map[string]any{
				"type": string(cfg.Type),
				"url":  cfg.URL,
			}

			if len(cfg.Headers) > 0 {
				entry["headers"] = cfg.Headers
			}

			result[name] = entry
		case *mcp.HTTPServerConfig:
			entry := map[string]any{
				"type": string(cfg.Type),
				"url":  cfg.URL,
			}

			if len(cfg.Headers) > 0 {
				entry["headers"] = cfg.Headers
			}

			result[name] = entry
		case *mcp.SdkServerConfig:
			entry := map[string]any{
				"type": string(cfg.Type),
			}

			if instance, ok := cfg.Instance.(mcp.ServerInstance); ok {
				entry["tools"] = instance.ListTools()
			}

			result[name] = entry
		}
	}

	return result
}

func extractOutputSchema(outputFormat map[string]any) map[string]any {
	if outputFormat == nil {
		return nil
	}

	formatType, _ := outputFormat["type"].(string)
	if formatType == "json_schema" {
		if s, ok := outputFormat["schema"].(map[string]any); ok {
			schema.EnforceAdditionalProperties(s)

			return s
		}

		return nil
	}

	if _, ok := outputFormat["properties"]; ok {
		schema.EnforceAdditionalProperties(outputFormat)

		return outputFormat
	}

	return nil
}

func mapPermissionToSandbox(permMode string) string {
	switch permMode {
	case "acceptEdits":
		return "workspace-write"
	case "bypassPermissions":
		return "danger-full-access"
	default:
		return ""
	}
}

func mapPermissionToApprovalPolicy(permMode string) string {
	switch permMode {
	case "bypassPermissions":
		return "never"
	case "default", "acceptEdits", "plan", "":
		return "on-request"
	default:
		return ""
	}
}

// getInitializeTimeout returns the initialize timeout.
func (s *Session) getInitializeTimeout() time.Duration {
	if s.options != nil && s.options.InitializeTimeout != nil {
		return *s.options.InitializeTimeout
	}

	if timeoutStr := os.Getenv("CODEX_INITIALIZE_TIMEOUT"); timeoutStr != "" {
		if timeoutSec, err := strconv.Atoi(timeoutStr); err == nil && timeoutSec > 0 {
			return time.Duration(timeoutSec) * time.Second
		}
	}

	return defaultInitializeTimeout
}

// NeedsInitialization returns true if the session has callbacks that require initialization.
func (s *Session) NeedsInitialization() bool {
	if s.options == nil {
		return false
	}

	return s.options.CanUseTool != nil ||
		s.options.OnUserInput != nil ||
		s.options.OnElicitation != nil ||
		len(s.sdkMcpServers) > 0 ||
		len(s.sdkDynamicTools) > 0
}

// GetInitializationResult returns a copy of the server initialization info.
func (s *Session) GetInitializationResult() map[string]any {
	s.initMu.RLock()
	defer s.initMu.RUnlock()

	if s.initializationResult == nil {
		return nil
	}

	return maps.Clone(s.initializationResult)
}

// GetSDKMCPServerNames returns the names of all registered SDK MCP servers.
func (s *Session) GetSDKMCPServerNames() []string {
	names := make([]string, 0, len(s.sdkMcpServers))
	for name := range s.sdkMcpServers {
		names = append(names, name)
	}

	return names
}

// GetSDKMCPServer returns a registered SDK MCP server by name.
func (s *Session) GetSDKMCPServer(name string) (mcp.ServerInstance, bool) {
	server, ok := s.sdkMcpServers[name]

	return server, ok
}

// HandleDynamicToolCall handles item/tool/call requests from the CLI for
// SDK-registered dynamic tools and MCP server tools.
func (s *Session) HandleDynamicToolCall(
	ctx context.Context,
	req *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	toolFullName, _ := req.Request["tool"].(string)
	arguments, _ := req.Request["arguments"].(map[string]any)

	callStart := time.Now()

	var toolSpan *agenttracer.Span

	obs := s.observer()
	if obs != nil {
		ctx, toolSpan = obs.StartToolSpan(ctx, toolFullName, "")
	}

	endToolSpan := func(outcome string) {
		if obs != nil {
			obs.RecordToolCallDuration(ctx, time.Since(callStart).Seconds(), toolFullName)
			obs.RecordToolCall(ctx, toolFullName, outcome)

			if toolSpan != nil {
				toolSpan.SetAttributes(observability.Outcome(outcome))
				toolSpan.End()
			}
		}
	}

	// Try plain name lookup in dynamic tools first.
	if tool, ok := s.sdkDynamicTools[toolFullName]; ok {
		result, err := s.executeDynamicTool(ctx, tool, arguments)

		outcome := "ok"
		if err != nil || (result != nil && result["success"] == false) {
			outcome = outcomeError
		}

		endToolSpan(outcome)

		return result, err
	}

	// Fall back to MCP server lookup for mcp__<server>__<tool> names.
	serverName, toolName, err := parseMCPToolName(toolFullName)
	if err != nil {
		endToolSpan(outcomeError)

		//nolint:nilerr // Error is encoded in the protocol response
		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": fmt.Sprintf("unknown tool: %s", toolFullName),
			}},
		}, nil
	}

	server, exists := s.sdkMcpServers[serverName]
	if !exists {
		endToolSpan(outcomeError)

		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": fmt.Sprintf("SDK MCP server not found: %s", serverName),
			}},
		}, nil
	}

	result, callErr := server.CallTool(ctx, toolName, arguments)
	if callErr != nil {
		endToolSpan(outcomeError)

		//nolint:nilerr // Error is encoded in the protocol response
		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": callErr.Error(),
			}},
		}, nil
	}

	isError, _ := result["is_error"].(bool)

	outcome := "ok"
	if isError {
		outcome = outcomeError
	}

	endToolSpan(outcome)

	contentItems := convertMCPContentToItems(result)

	return map[string]any{
		"success":      !isError,
		"contentItems": contentItems,
	}, nil
}

// executeDynamicTool calls a dynamic tool handler and formats the result
// as the protocol response.
func (s *Session) executeDynamicTool(
	ctx context.Context,
	tool *config.DynamicTool,
	arguments map[string]any,
) (map[string]any, error) {
	result, err := tool.Handler(ctx, arguments)
	if err != nil {
		//nolint:nilerr // Error is encoded in the protocol response
		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": err.Error(),
			}},
		}, nil
	}

	data, err := json.Marshal(result)
	if err != nil {
		return map[string]any{
			"success": false,
			"contentItems": []map[string]any{{
				"type": "inputText",
				"text": fmt.Sprintf("failed to marshal tool result: %v", err),
			}},
		}, nil
	}

	return map[string]any{
		"success": true,
		"contentItems": []map[string]any{{
			"type": "inputText",
			"text": string(data),
		}},
	}, nil
}

// parseMCPToolName splits "mcp__<server>__<tool>" into server and tool names.
func parseMCPToolName(fullName string) (serverName, toolName string, err error) {
	const prefix = "mcp__"
	if len(fullName) <= len(prefix) || fullName[:len(prefix)] != prefix {
		return "", "", fmt.Errorf("invalid MCP tool name format: %s", fullName)
	}

	rest := fullName[len(prefix):]
	idx := 0

	for i := 0; i+1 < len(rest); i++ {
		if rest[i] == '_' && rest[i+1] == '_' {
			idx = i

			break
		}
	}

	if idx == 0 {
		return "", "", fmt.Errorf("invalid MCP tool name format (missing server/tool separator): %s", fullName)
	}

	return rest[:idx], rest[idx+2:], nil
}

// convertMCPContentToItems converts MCP result content entries to the
// DynamicToolCallResponse contentItems format.
func convertMCPContentToItems(result map[string]any) []map[string]any {
	content, ok := result["content"].([]map[string]any)
	if !ok {
		// Try []any (common from JSON unmarshalling).
		if contentAny, ok := result["content"].([]any); ok {
			items := make([]map[string]any, 0, len(contentAny))
			for _, entry := range contentAny {
				if entryMap, ok := entry.(map[string]any); ok {
					items = append(items, convertMCPContentEntry(entryMap))
				}
			}

			return items
		}

		return []map[string]any{}
	}

	items := make([]map[string]any, 0, len(content))
	for _, entry := range content {
		items = append(items, convertMCPContentEntry(entry))
	}

	return items
}

// convertMCPContentEntry converts a single MCP content entry to a
// DynamicToolCallResponse content item, handling both text and image types.
func convertMCPContentEntry(entry map[string]any) map[string]any {
	entryType, _ := entry["type"].(string)

	if entryType == "image" {
		imageURL, _ := entry["image_url"].(string)
		if imageURL == "" {
			imageURL, _ = entry["url"].(string)
		}

		return map[string]any{
			"type":     "inputImage",
			"imageUrl": imageURL,
		}
	}

	text, _ := entry["text"].(string)

	return map[string]any{
		"type": "inputText",
		"text": text,
	}
}

// HandleRequestUserInput handles item/tool/requestUserInput requests from the CLI.
// It parses the request into typed userinput types, invokes the OnUserInput callback,
// and serializes the response back to the wire format.
func (s *Session) HandleRequestUserInput(
	ctx context.Context,
	req *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if s.options == nil || s.options.OnUserInput == nil {
		return map[string]any{"answers": map[string]any{}}, nil
	}

	parsed, err := parseUserInputRequest(req)
	if err != nil {
		return nil, fmt.Errorf("parse user input request: %w", err)
	}

	resp, err := s.options.OnUserInput(ctx, parsed)
	if err != nil {
		return nil, err
	}

	return serializeUserInputResponse(resp), nil
}

// parseUserInputRequest extracts typed userinput.Request from the wire format.
func parseUserInputRequest(req *ControlRequest) (*userinput.Request, error) {
	result := &userinput.Request{}
	result.ItemID, _ = req.Request["item_id"].(string)
	result.ThreadID, _ = req.Request["thread_id"].(string)
	result.TurnID, _ = req.Request["turn_id"].(string)

	payload, err := json.Marshal(req.Request)
	if err != nil {
		return nil, fmt.Errorf("marshal user input request: %w", err)
	}

	result.Audit = &message.AuditEnvelope{
		EventType: "item_tool/requestUserInput",
		Subtype:   "request",
		Payload:   payload,
	}

	questionsRaw, _ := req.Request["questions"].([]any)
	if len(questionsRaw) == 0 {
		return result, nil
	}

	result.Questions = make([]userinput.Question, 0, len(questionsRaw))

	for _, qRaw := range questionsRaw {
		qMap, ok := qRaw.(map[string]any)
		if !ok {
			continue
		}

		q := userinput.Question{}
		q.ID, _ = qMap["id"].(string)
		q.Header, _ = qMap["header"].(string)
		q.Question, _ = qMap["question"].(string)

		q.MultiSelect, _ = qMap["multiSelect"].(bool)
		if !q.MultiSelect {
			if multiSelectSnake, ok := qMap["multi_select"].(bool); ok {
				q.MultiSelect = multiSelectSnake
			}
		}

		q.IsOther, _ = qMap["is_other"].(bool)
		q.IsSecret, _ = qMap["is_secret"].(bool)

		if optionsRaw, ok := qMap["options"].([]any); ok {
			q.Options = make([]userinput.QuestionOption, 0, len(optionsRaw))

			for _, oRaw := range optionsRaw {
				oMap, ok := oRaw.(map[string]any)
				if !ok {
					continue
				}

				opt := userinput.QuestionOption{}
				opt.Label, _ = oMap["label"].(string)
				opt.Description, _ = oMap["description"].(string)

				q.Options = append(q.Options, opt)
			}
		}

		result.Questions = append(result.Questions, q)
	}

	return result, nil
}

// serializeUserInputResponse converts a typed Response into the wire format.
func serializeUserInputResponse(resp *userinput.Response) map[string]any {
	if resp == nil || len(resp.Answers) == 0 {
		return map[string]any{"answers": map[string]any{}}
	}

	answers := make(map[string]any, len(resp.Answers))

	for qID, answer := range resp.Answers {
		if answer == nil {
			continue
		}

		answers[qID] = map[string]any{
			"answers": answer.Answers,
		}
	}

	payload, err := json.Marshal(map[string]any{"answers": answers})
	if err == nil {
		resp.Audit = &message.AuditEnvelope{
			EventType: "item_tool/requestUserInput",
			Subtype:   "response",
			Payload:   payload,
		}
	}

	return map[string]any{"answers": answers}
}

// HandleCanUseTool is called by CLI before tool use.
func (s *Session) HandleCanUseTool(
	ctx context.Context,
	req *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	toolName, _ := req.Request["tool_name"].(string)
	toolName = s.normalizePermissionToolName(toolName)
	input, _ := req.Request["input"].(map[string]any)

	// Normalize command approval requests into the public tool callback shape.
	if toolName == "" {
		if command, ok := req.Request["command"].(string); ok && command != "" {
			toolName = "Bash"

			if input == nil {
				input = map[string]any{
					"command": command,
				}
				if cwd, ok := req.Request["cwd"].(string); ok && cwd != "" {
					input["cwd"] = cwd
				}
			}
		} else if command, ok := commandArrayToString(req.Request["command"]); ok && command != "" {
			toolName = "Bash"

			if input == nil {
				input = map[string]any{
					"command": command,
				}
				if cwd, ok := req.Request["cwd"].(string); ok && cwd != "" {
					input["cwd"] = cwd
				}
			}
		}
	}

	if s.options == nil || s.options.CanUseTool == nil {
		return map[string]any{
			"decision": "accept",
		}, nil
	}

	var suggestions []*permission.Update
	if suggestionsData, ok := req.Request["suggestions"].([]any); ok {
		suggestions = make([]*permission.Update, 0, len(suggestionsData))

		for _, sg := range suggestionsData {
			if suggestionMap, ok := sg.(map[string]any); ok {
				update := &permission.Update{}
				if t, ok := suggestionMap["type"].(string); ok {
					update.Type = permission.UpdateType(t)
				}

				suggestions = append(suggestions, update)
			}
		}
	}

	permCtx := &permission.Context{
		Suggestions: suggestions,
	}

	decision, err := s.options.CanUseTool(ctx, toolName, input, permCtx)
	if err != nil {
		return nil, err
	}

	switch decision.(type) {
	case *permission.ResultAllow:
		return map[string]any{
			"decision": "accept",
		}, nil

	case *permission.ResultDeny:
		// Record the denied tool call so dashboards can distinguish
		// denials from errors.
		if obs := s.observer(); obs != nil {
			obs.RecordToolCall(ctx, toolName, "denied")
		}

		return map[string]any{
			"decision": "decline",
		}, nil

	default:
		return nil, fmt.Errorf(
			"tool permission callback must return *ResultAllow or *ResultDeny, got %T",
			decision,
		)
	}
}

func (s *Session) normalizePermissionToolName(toolName string) string {
	const sdkMCPPrefix = "sdkmcp__"

	if !strings.HasPrefix(toolName, sdkMCPPrefix) {
		return toolName
	}

	rest := strings.TrimPrefix(toolName, sdkMCPPrefix)

	idx := strings.Index(rest, "__")
	if idx <= 0 {
		return toolName
	}

	serverName := rest[:idx]
	if _, ok := s.sdkMcpServers[serverName]; !ok {
		return toolName
	}

	return "mcp__" + rest
}

// HandleFileChangeApproval handles item/fileChange/requestApproval requests.
// It routes through the CanUseTool callback if set, otherwise auto-accepts.
func (s *Session) HandleFileChangeApproval(
	ctx context.Context,
	req *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if s.options == nil || s.options.CanUseTool == nil {
		return map[string]any{"decision": "accept"}, nil
	}

	toolName := inferFileApprovalToolName(req.Request)

	input := make(map[string]any, 4)
	if itemID, ok := req.Request["itemId"].(string); ok {
		input["itemId"] = itemID
	}

	if grantRoot, ok := req.Request["grantRoot"].(string); ok {
		input["grantRoot"] = grantRoot
	}

	if reason, ok := req.Request["reason"].(string); ok {
		input["reason"] = reason
	}

	if fileChanges, ok := req.Request["fileChanges"].(map[string]any); ok && len(fileChanges) > 0 {
		input["fileChanges"] = fileChanges
	}

	decision, err := s.options.CanUseTool(ctx, toolName, input, &permission.Context{})
	if err != nil {
		return nil, err
	}

	switch decision.(type) {
	case *permission.ResultAllow:
		return map[string]any{"decision": "accept"}, nil
	case *permission.ResultDeny:
		return map[string]any{"decision": "decline"}, nil
	default:
		return nil, fmt.Errorf(
			"tool permission callback must return *ResultAllow or *ResultDeny, got %T",
			decision,
		)
	}
}

// HandlePermissionsApproval handles item/permissions/requestApproval requests.
// It routes through the CanUseTool callback if set, otherwise auto-approves
// with the requested permissions scoped to the current turn.
func (s *Session) HandlePermissionsApproval(
	ctx context.Context,
	req *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	permissions, _ := req.Request["permissions"].(map[string]any)

	if s.options == nil || s.options.CanUseTool == nil {
		return map[string]any{
			"permissions": permissions,
			"scope":       "turn",
		}, nil
	}

	input := make(map[string]any, 2)

	if permissions != nil {
		input["permissions"] = permissions
	}

	if reason, ok := req.Request["reason"].(string); ok {
		input["reason"] = reason
	}

	decision, err := s.options.CanUseTool(ctx, "Permissions", input, &permission.Context{})
	if err != nil {
		return nil, err
	}

	switch decision.(type) {
	case *permission.ResultAllow:
		return map[string]any{
			"permissions": permissions,
			"scope":       "turn",
		}, nil
	case *permission.ResultDeny:
		return map[string]any{
			"permissions": map[string]any{},
			"scope":       "turn",
		}, nil
	default:
		return nil, fmt.Errorf(
			"tool permission callback must return *ResultAllow or *ResultDeny, got %T",
			decision,
		)
	}
}

// HandleMCPElicitation handles mcpServer/elicitation/request requests.
// It parses the request into typed elicitation types, invokes the OnElicitation
// callback, and serializes the response back to the wire format.
// If no callback is set, elicitation requests are auto-declined.
func (s *Session) HandleMCPElicitation(
	ctx context.Context,
	req *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if s.options == nil || s.options.OnElicitation == nil {
		return map[string]any{"action": string(elicitation.ActionDecline)}, nil
	}

	parsed := parseElicitationRequest(req)

	resp, err := s.options.OnElicitation(ctx, parsed)
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return map[string]any{"action": string(elicitation.ActionDecline)}, nil
	}

	result := map[string]any{
		"action": string(resp.Action),
	}

	if resp.Content != nil {
		result["content"] = resp.Content
	}

	return result, nil
}

// parseElicitationRequest extracts a typed elicitation.Request from the wire format.
func parseElicitationRequest(req *ControlRequest) *elicitation.Request {
	result := &elicitation.Request{}
	result.MCPServerName, _ = req.Request["serverName"].(string)
	result.Message, _ = req.Request["message"].(string)
	result.ThreadID, _ = req.Request["threadId"].(string)

	if modeStr, ok := req.Request["mode"].(string); ok {
		m := elicitation.Mode(modeStr)
		result.Mode = &m
	}

	if urlStr, ok := req.Request["url"].(string); ok {
		result.URL = &urlStr
	}

	if eid, ok := req.Request["elicitationId"].(string); ok {
		result.ElicitationID = &eid
	}

	if turnID, ok := req.Request["turnId"].(string); ok {
		result.TurnID = &turnID
	}

	if schema, ok := req.Request["requestedSchema"].(map[string]any); ok {
		result.RequestedSchema = schema
	}

	payload, err := json.Marshal(req.Request)
	if err == nil {
		result.Audit = &message.AuditEnvelope{
			EventType: "mcpServer_elicitation/request",
			Subtype:   "request",
			Payload:   payload,
		}
	}

	return result
}

// HandleChatGPTAuthTokensRefresh handles account/chatgptAuthTokens/refresh
// requests used by app-server external-auth mode.
func (s *Session) HandleChatGPTAuthTokensRefresh(
	ctx context.Context,
	_ *ControlRequest,
) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	accessToken := firstNonEmptyEnv(
		"CODEX_CHATGPT_ACCESS_TOKEN",
		"CHATGPT_ACCESS_TOKEN",
	)
	accountID := firstNonEmptyEnv(
		"CODEX_CHATGPT_ACCOUNT_ID",
		"CHATGPT_ACCOUNT_ID",
	)

	if accessToken == "" || accountID == "" {
		return nil, fmt.Errorf(
			"chatgpt auth token refresh requested but no external auth tokens are configured; use Codex managed auth or set CODEX_CHATGPT_ACCESS_TOKEN and CODEX_CHATGPT_ACCOUNT_ID",
		)
	}

	resp := map[string]any{
		"accessToken":      accessToken,
		"chatgptAccountId": accountID,
	}

	if planType := firstNonEmptyEnv("CODEX_CHATGPT_PLAN_TYPE", "CHATGPT_PLAN_TYPE"); planType != "" {
		resp["chatgptPlanType"] = planType
	}

	return resp, nil
}

func commandArrayToString(value any) (string, bool) {
	switch v := value.(type) {
	case []string:
		if len(v) == 0 {
			return "", false
		}

		return strings.Join(v, " "), true
	case []any:
		if len(v) == 0 {
			return "", false
		}

		parts := make([]string, 0, len(v))
		for _, part := range v {
			s, ok := part.(string)
			if !ok {
				return "", false
			}

			parts = append(parts, s)
		}

		return strings.Join(parts, " "), true
	default:
		return "", false
	}
}

func inferFileApprovalToolName(request map[string]any) string {
	fileChanges, ok := request["fileChanges"].(map[string]any)
	if !ok {
		return "Edit"
	}

	for _, raw := range fileChanges {
		change, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		changeType, _ := change["type"].(string)
		if changeType == "add" || changeType == "create" {
			return "Write"
		}
	}

	return "Edit"
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return ""
}
