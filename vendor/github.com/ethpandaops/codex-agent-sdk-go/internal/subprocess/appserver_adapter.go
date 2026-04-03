package subprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/schema"
)

// appServerRPC defines the JSON-RPC operations that AppServerAdapter
// needs from the inner transport. AppServerTransport implements this,
// and tests can provide a mock.
type appServerRPC interface {
	Start(ctx context.Context) error
	Close() error
	IsReady() bool
	SendRequest(ctx context.Context, method string, params any) (*RPCResponse, error)
	SendResponse(id int64, result json.RawMessage, rpcErr *RPCError) error
	Notifications() <-chan *RPCNotification
	Requests() <-chan *RPCIncomingRequest
}

// AppServerAdapter bridges the control_request/control_response protocol
// used by Controller/Session to the JSON-RPC 2.0 protocol spoken by
// codex app-server. It implements config.Transport.
type AppServerAdapter struct {
	log   *slog.Logger
	inner appServerRPC

	messages chan map[string]any
	errs     chan error
	done     chan struct{}

	mu       sync.Mutex
	threadID string
	turnID   string

	modelOverride             *string
	approvalPolicyOverride    *string
	sandboxPolicyOverride     map[string]any
	effortOverride            *string
	outputSchemaOverride      any
	collaborationModeOverride map[string]any

	// lastTokenUsage caches the most recent token usage data from
	// thread/tokenUsage/updated, injected into turn/completed if no
	// inline usage is present.
	lastTokenUsage map[string]any

	// lastAssistantText caches the latest completed assistant text for a turn.
	// Used as a fallback result payload when turn/completed does not include one.
	lastAssistantText       string
	lastAssistantTextByTurn map[string]string

	// currentTurnHasOutputSchema tracks whether the active turn requested
	// structured output, allowing the adapter to parse JSON final text back
	// into ResultMessage.StructuredOutput when app-server omits a dedicated field.
	currentTurnHasOutputSchema bool
	turnHasOutputSchema        map[string]bool

	// includePartialMessages controls whether streaming deltas are emitted
	// as stream_event messages. When false, delta notifications are suppressed.
	includePartialMessages bool

	// pendingRPCRequests maps synthetic request_id strings to JSON-RPC IDs
	// for server-to-client requests (hooks/MCP).
	pendingRPCRequests map[string]int64
	sdkMCPServerNames  map[string]struct{}

	// messagesClosed is set before closing the messages channel so that
	// senders on other goroutines can avoid a send-on-closed-channel panic.
	messagesClosed atomic.Bool

	wg sync.WaitGroup
}

// Compile-time verification that AppServerAdapter implements Transport.
var _ config.Transport = (*AppServerAdapter)(nil)

// NewAppServerAdapter creates a new adapter that wraps an AppServerTransport.
func NewAppServerAdapter(
	log *slog.Logger,
	opts *config.Options,
) *AppServerAdapter {
	return &AppServerAdapter{
		log:                     log.With(slog.String("component", "appserver_adapter")),
		inner:                   NewAppServerTransport(log, opts),
		messages:                make(chan map[string]any, 128),
		errs:                    make(chan error, 4),
		done:                    make(chan struct{}),
		includePartialMessages:  opts.IncludePartialMessages,
		pendingRPCRequests:      make(map[string]int64, 8),
		lastAssistantTextByTurn: make(map[string]string, 8),
		turnHasOutputSchema:     make(map[string]bool, 8),
		sdkMCPServerNames:       make(map[string]struct{}, 8),
	}
}

// Start initializes the inner transport (JSON-RPC handshake) and starts
// the adapter read loop that translates notifications into exec-event format.
func (a *AppServerAdapter) Start(ctx context.Context) error {
	if err := a.inner.Start(ctx); err != nil {
		return err
	}

	a.wg.Add(1)

	go a.readLoop()

	return nil
}

// ReadMessages returns channels populated by the adapter read loop with
// messages in exec-event format that message.Parse() understands.
func (a *AppServerAdapter) ReadMessages(
	_ context.Context,
) (<-chan map[string]any, <-chan error) {
	return a.messages, a.errs
}

// SendMessage intercepts outgoing control protocol messages and translates
// them into JSON-RPC calls on the inner transport.
func (a *AppServerAdapter) SendMessage(ctx context.Context, data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal outgoing message: %w", err)
	}

	msgType, _ := raw["type"].(string)

	switch msgType {
	case "control_request":
		return a.handleControlRequest(ctx, raw)
	case "control_response":
		return a.handleControlResponse(raw)
	case "user":
		return a.handleUserMessage(ctx, raw)
	default:
		a.log.DebugContext(ctx, "passing through unknown message type",
			slog.String("type", msgType),
		)

		return nil
	}
}

// Close shuts down the adapter and inner transport.
func (a *AppServerAdapter) Close() error {
	select {
	case <-a.done:
		return nil
	default:
		close(a.done)
	}

	err := a.inner.Close()
	a.wg.Wait()

	return err
}

// IsReady delegates to the inner transport.
func (a *AppServerAdapter) IsReady() bool {
	return a.inner.IsReady()
}

// EndInput is a no-op for app-server sessions which stay alive for
// multi-turn interaction.
func (a *AppServerAdapter) EndInput() error {
	return nil
}

// handleControlRequest translates outgoing control_request messages to
// JSON-RPC calls.
func (a *AppServerAdapter) handleControlRequest(
	ctx context.Context,
	raw map[string]any,
) error {
	requestData, _ := raw["request"].(map[string]any)
	if requestData == nil {
		return fmt.Errorf("control_request missing 'request' field")
	}

	subtype, _ := requestData["subtype"].(string)
	requestID, _ := raw["request_id"].(string)

	switch subtype {
	case "initialize":
		return a.handleInitialize(ctx, requestID, requestData)
	case "interrupt":
		return a.handleInterrupt(ctx, requestID)
	case "set_permission_mode":
		return a.handleSetPermissionMode(requestID, requestData)
	case "set_model":
		return a.handleSetModel(requestID, requestData)
	case "mcp_status":
		return a.handleMCPStatus(ctx, requestID)
	case "list_models":
		return a.handleListModels(ctx, requestID)
	case "rewind_files":
		return a.handleRewindFiles(requestID)
	default:
		a.log.DebugContext(ctx, "unsupported control_request subtype",
			slog.String("subtype", subtype),
		)

		a.injectErrorControlResponse(
			requestID,
			fmt.Sprintf("%s: %s", sdkerrors.ErrUnsupportedControlRequest, subtype),
		)

		return nil
	}
}

// handleInitialize translates an "initialize" control_request into a
// thread/start JSON-RPC call and fabricates a control_response.
func (a *AppServerAdapter) handleInitialize(
	ctx context.Context,
	requestID string,
	requestData map[string]any,
) error {
	method, params, turnOverrides, err := buildInitializeRPC(requestData)
	if err != nil {
		a.injectErrorControlResponse(requestID, err.Error())

		return nil
	}

	resp, err := a.inner.SendRequest(ctx, method, params)
	if err != nil {
		a.injectErrorControlResponse(requestID, fmt.Sprintf("%s RPC: %v", method, err))

		return nil
	}

	responsePayload := map[string]any{}

	if resp.Result != nil {
		var result map[string]any
		if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr == nil {
			a.log.DebugContext(ctx, "initialize rpc response",
				slog.String("method", method),
				slog.Any("result", result),
			)

			responsePayload = result

			if tid := extractThreadID(result); tid != "" {
				a.mu.Lock()
				a.threadID = tid
				a.mu.Unlock()
			}
		}
	}

	// If collaboration mode is set but missing a model, backfill from
	// the thread/start response so the CLI doesn't reject turn/start.
	if cm := turnOverrides.collaborationMode; cm != nil {
		if settings, ok := cm["settings"].(map[string]any); ok {
			if _, hasModel := settings["model"]; !hasModel {
				if respModel, ok := responsePayload["model"].(string); ok && respModel != "" {
					settings["model"] = respModel
				}
			}
		}
	}

	a.mu.Lock()
	a.effortOverride = turnOverrides.effort
	a.outputSchemaOverride = cloneAnyValue(turnOverrides.outputSchema)
	a.collaborationModeOverride = cloneAnyMap(turnOverrides.collaborationMode)
	a.sdkMCPServerNames = cloneStringSet(turnOverrides.sdkMCPServerNames)
	a.mu.Unlock()

	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "success",
		"request_id": requestID,
		"response":   responsePayload,
	})

	return nil
}

type initializeTurnOverrides struct {
	collaborationMode map[string]any
	effort            *string
	outputSchema      any
	sdkMCPServerNames map[string]struct{}
}

const sdkMCPDynamicToolPrefix = "sdkmcp__"

//nolint:gocyclo // initialization normalization intentionally handles many option variants.
func buildInitializeRPC(
	requestData map[string]any,
) (string, map[string]any, initializeTurnOverrides, error) {
	resumeID, _ := requestData["resume"].(string)
	forkSession, _ := requestData["forkSession"].(bool)
	continueConversation, _ := requestData["continueConversation"].(bool)

	turnOverrides := initializeTurnOverrides{}

	params := make(map[string]any, 24)
	if model, ok := requestData["model"].(string); ok && model != "" {
		params["model"] = model
	}

	if cwd, ok := requestData["cwd"].(string); ok && cwd != "" {
		params["cwd"] = cwd
	}

	if configMap, ok := requestData["config"].(map[string]any); ok && len(configMap) > 0 {
		params["config"] = configMap
	}

	if approvalRaw, ok := requestData["approvalPolicy"].(string); ok && approvalRaw != "" {
		approvalPolicy, err := normalizeApprovalPolicy(approvalRaw)
		if err != nil {
			return "", nil, initializeTurnOverrides{}, err
		}

		params["approvalPolicy"] = approvalPolicy
	}

	if sandboxRaw, ok := requestData["sandbox"].(string); ok && sandboxRaw != "" {
		sandboxMode, err := normalizeSandboxMode(sandboxRaw)
		if err != nil {
			return "", nil, initializeTurnOverrides{}, err
		}

		params["sandbox"] = sandboxMode
	}

	if effortRaw, ok := requestData["reasoningEffort"].(string); ok && effortRaw != "" {
		effort, err := normalizeEffort(effortRaw)
		if err != nil {
			return "", nil, initializeTurnOverrides{}, err
		}

		turnOverrides.effort = &effort
	}

	if permMode, ok := requestData["permissionMode"].(string); ok && permMode == permissionModePlan {
		model, _ := requestData["model"].(string)
		turnOverrides.collaborationMode = buildCollaborationMode(permissionModePlan, model)
	}

	// Explicit developerInstructions takes precedence over systemPrompt mapping.
	if devInstr, ok := requestData["developerInstructions"].(string); ok && devInstr != "" {
		params["developerInstructions"] = devInstr
	}

	// SystemPrompt maps to developerInstructions if no explicit one was set.
	if _, hasDI := params["developerInstructions"]; !hasDI {
		if systemPrompt, ok := requestData["systemPrompt"].(string); ok && systemPrompt != "" {
			params["developerInstructions"] = systemPrompt
		}
	}

	// Claude-style systemPromptPreset is emulated by forwarding its append text
	// as developer instructions when no explicit system prompt or developer
	// instructions were provided.
	if _, hasDI := params["developerInstructions"]; !hasDI {
		if preset, ok := requestData["systemPromptPreset"].(map[string]any); ok {
			if appendText, ok := preset["append"].(string); ok && appendText != "" {
				params["developerInstructions"] = appendText
			}
		}
	}

	if personality, ok := requestData["personality"].(string); ok && personality != "" {
		params["personality"] = personality
	}

	if serviceTier, ok := requestData["serviceTier"].(string); ok && serviceTier != "" {
		params["serviceTier"] = serviceTier
	}

	// Pass through initialize fields that are part of the current SDK surface.
	passthroughKeys := []string{
		"allowedTools",
		"disallowedTools",
		"tools",
		"addDirs",
		"dynamicTools",
		"permissionPromptToolName",
	}
	for _, key := range passthroughKeys {
		value, ok := requestData[key]
		if !ok || value == nil {
			continue
		}

		params[key] = cloneAnyValue(value)
	}

	if rawMCPServers, ok := requestData["mcpServers"]; ok && rawMCPServers != nil {
		mcpServers, dynamicTools, sdkServerNames := normalizeAppServerMCPServers(rawMCPServers)
		turnOverrides.sdkMCPServerNames = cloneStringSet(sdkServerNames)

		if err := validateSDKMCPDynamicToolCollisions(params["dynamicTools"], dynamicTools); err != nil {
			return "", nil, initializeTurnOverrides{}, err
		}

		if len(mcpServers) > 0 {
			params["mcpServers"] = mcpServers
		}

		if len(dynamicTools) > 0 {
			params["dynamicTools"] = append(dynamicToolListFromAny(params["dynamicTools"]), dynamicTools...)
		}

		rewriteAppServerToolList(params, "tools", sdkServerNames)
		rewriteAppServerToolList(params, "allowedTools", sdkServerNames)
		rewriteAppServerToolList(params, "disallowedTools", sdkServerNames)
	}

	if outputSchema, ok := requestData["outputSchema"]; ok {
		normalizedOutputSchema, err := normalizeOutputSchema(outputSchema)
		if err != nil {
			return "", nil, initializeTurnOverrides{}, err
		}

		turnOverrides.outputSchema = normalizedOutputSchema
	}

	if permissionPromptToolName, ok := requestData["permissionPromptToolName"].(string); ok &&
		permissionPromptToolName != "" && permissionPromptToolName != "stdio" {
		return "", nil, initializeTurnOverrides{}, fmt.Errorf(
			"%w: permissionPromptToolName %q is unsupported by codex app-server",
			sdkerrors.ErrUnsupportedOption,
			permissionPromptToolName,
		)
	}

	if continueConversation && resumeID == "" {
		return "", nil, initializeTurnOverrides{}, fmt.Errorf(
			"%w: continueConversation requires resume in app-server mode",
			sdkerrors.ErrUnsupportedOption,
		)
	}

	if resumeID != "" {
		params["threadId"] = resumeID
		if forkSession {
			return "thread/fork", params, turnOverrides, nil
		}

		return "thread/resume", params, turnOverrides, nil
	}

	return "thread/start", params, turnOverrides, nil
}

func validateSDKMCPDynamicToolCollisions(existing any, generated []map[string]any) error {
	existingTools := dynamicToolListFromAny(existing)
	if len(existingTools) == 0 && len(generated) == 0 {
		return nil
	}

	generatedNames := make(map[string]struct{}, len(generated))
	for _, tool := range generated {
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}

		generatedNames[name] = struct{}{}
	}

	for _, tool := range existingTools {
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}

		if strings.HasPrefix(name, sdkMCPDynamicToolPrefix) {
			return fmt.Errorf(
				"%w: dynamic tool name %q uses reserved SDK MCP prefix %q",
				sdkerrors.ErrUnsupportedOption,
				name,
				sdkMCPDynamicToolPrefix,
			)
		}

		if _, ok := generatedNames[name]; ok {
			return fmt.Errorf(
				"%w: dynamic tool name %q collides with generated SDK MCP tool",
				sdkerrors.ErrUnsupportedOption,
				name,
			)
		}
	}

	return nil
}

func normalizeAppServerMCPServers(raw any) (map[string]any, []map[string]any, map[string]struct{}) {
	servers, ok := raw.(map[string]any)
	if !ok || len(servers) == 0 {
		return nil, nil, nil
	}

	normalizedServers := make(map[string]any, len(servers))
	dynamicTools := make([]map[string]any, 0, len(servers))
	sdkServerNames := make(map[string]struct{}, len(servers))

	for serverName, rawServer := range servers {
		server, ok := rawServer.(map[string]any)
		if !ok || len(server) == 0 {
			continue
		}

		serverType, _ := server["type"].(string)
		if serverType != "sdk" {
			normalizedServers[serverName] = cloneAnyMap(server)

			continue
		}

		sdkServerNames[serverName] = struct{}{}

		sdkTools, ok := server["tools"].([]map[string]any)
		if !ok {
			if toolList, ok := server["tools"].([]any); ok {
				sdkTools = make([]map[string]any, 0, len(toolList))
				for _, rawTool := range toolList {
					toolMap, ok := rawTool.(map[string]any)
					if !ok {
						continue
					}

					sdkTools = append(sdkTools, toolMap)
				}
			}
		}

		dynamicTools = append(dynamicTools, sdkMCPServerToolsToDynamicTools(serverName, sdkTools)...)
	}

	if len(normalizedServers) == 0 {
		normalizedServers = nil
	}

	if len(dynamicTools) == 0 {
		dynamicTools = nil
	}

	if len(sdkServerNames) == 0 {
		sdkServerNames = nil
	}

	return normalizedServers, dynamicTools, sdkServerNames
}

func sdkMCPServerToolsToDynamicTools(serverName string, tools []map[string]any) []map[string]any {
	if len(tools) == 0 {
		return nil
	}

	dynamicTools := make([]map[string]any, 0, len(tools))

	for _, tool := range tools {
		toolName, _ := tool["name"].(string)
		if toolName == "" {
			continue
		}

		dynamicTool := map[string]any{
			"name": sdkMCPDynamicToolName(serverName, toolName),
		}

		description, _ := tool["description"].(string)
		if description != "" {
			dynamicTool["description"] = description
		} else {
			dynamicTool["description"] = fmt.Sprintf("MCP tool %s on server %s", toolName, serverName)
		}

		if inputSchema, ok := tool["inputSchema"]; ok && inputSchema != nil {
			dynamicTool["inputSchema"] = cloneAnyValue(inputSchema)
		} else {
			dynamicTool["inputSchema"] = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}

		dynamicTools = append(dynamicTools, dynamicTool)
	}

	return dynamicTools
}

func rewriteAppServerToolList(params map[string]any, key string, sdkServerNames map[string]struct{}) {
	tools := stringListFromAny(params[key])

	if len(tools) == 0 || len(sdkServerNames) == 0 {
		return
	}

	rewritten := make([]string, 0, len(tools))

	for _, toolName := range tools {
		serverName, rawToolName, err := parsePublicMCPToolName(toolName)
		if err != nil {
			rewritten = append(rewritten, toolName)

			continue
		}

		if _, ok := sdkServerNames[serverName]; ok {
			rewritten = append(rewritten, sdkMCPDynamicToolName(serverName, rawToolName))
		} else {
			rewritten = append(rewritten, toolName)
		}
	}

	params[key] = rewritten
}

func dynamicToolListFromAny(value any) []map[string]any {
	switch v := value.(type) {
	case nil:
		return nil
	case []map[string]any:
		return cloneMapSlice(v)
	case []any:
		tools := make([]map[string]any, 0, len(v))
		for _, raw := range v {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}

			tools = append(tools, cloneAnyMap(tool))
		}

		return tools
	default:
		return nil
	}
}

func stringListFromAny(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		out := make([]string, len(v))
		copy(out, v)

		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, raw := range v {
			s, ok := raw.(string)
			if !ok {
				continue
			}

			out = append(out, s)
		}

		return out
	default:
		return nil
	}
}

func sdkMCPDynamicToolName(serverName, toolName string) string {
	return fmt.Sprintf("%s%s__%s", sdkMCPDynamicToolPrefix, serverName, toolName)
}

func internalToPublicToolName(name string, sdkServerNames map[string]struct{}) string {
	if !strings.HasPrefix(name, sdkMCPDynamicToolPrefix) {
		return name
	}

	if len(sdkServerNames) == 0 {
		return name
	}

	rest := strings.TrimPrefix(name, sdkMCPDynamicToolPrefix)

	idx := strings.Index(rest, "__")
	if idx <= 0 {
		return name
	}

	serverName := rest[:idx]
	if _, ok := sdkServerNames[serverName]; !ok {
		return name
	}

	return "mcp__" + rest
}

func parsePublicMCPToolName(fullName string) (serverName, toolName string, err error) {
	const prefix = "mcp__"
	if len(fullName) <= len(prefix) || fullName[:len(prefix)] != prefix {
		return "", "", fmt.Errorf("invalid MCP tool name format: %s", fullName)
	}

	rest := fullName[len(prefix):]

	idx := strings.Index(rest, "__")
	if idx <= 0 {
		return "", "", fmt.Errorf("invalid MCP tool name format (missing server/tool separator): %s", fullName)
	}

	return rest[:idx], rest[idx+2:], nil
}

// handleInterrupt translates an "interrupt" control_request into a
// turn/interrupt JSON-RPC call.
func (a *AppServerAdapter) handleInterrupt(
	ctx context.Context,
	requestID string,
) error {
	a.mu.Lock()
	turnID := a.turnID
	threadID := a.threadID
	a.mu.Unlock()

	params := map[string]any{}
	if threadID != "" {
		params["threadId"] = threadID
	}

	if turnID != "" {
		params["turnId"] = turnID
	}

	_, err := a.inner.SendRequest(ctx, "turn/interrupt", params)
	if err != nil {
		a.log.WarnContext(ctx, "turn/interrupt failed",
			slog.String("error", err.Error()),
		)
	}

	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "success",
		"request_id": requestID,
		"response":   map[string]any{},
	})

	return nil
}

func (a *AppServerAdapter) handleSetPermissionMode(
	requestID string,
	requestData map[string]any,
) error {
	mode, _ := requestData["mode"].(string)

	approvalPolicy, sandboxPolicy, err := permissionModeToTurnOverrides(mode)
	if err != nil {
		a.injectErrorControlResponse(requestID, err.Error())

		return nil
	}

	a.mu.Lock()
	if approvalPolicy == "" {
		a.approvalPolicyOverride = nil
	} else {
		ap := approvalPolicy
		a.approvalPolicyOverride = &ap
	}

	if sandboxPolicy == nil {
		a.sandboxPolicyOverride = nil
	} else {
		cloned := make(map[string]any, len(sandboxPolicy))
		maps.Copy(cloned, sandboxPolicy)

		a.sandboxPolicyOverride = cloned
	}

	if mode == permissionModePlan {
		var model string
		if a.modelOverride != nil {
			model = *a.modelOverride
		}

		a.collaborationModeOverride = buildCollaborationMode(permissionModePlan, model)
	} else {
		a.collaborationModeOverride = nil
	}
	a.mu.Unlock()

	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "success",
		"request_id": requestID,
		"response":   map[string]any{},
	})

	return nil
}

func (a *AppServerAdapter) handleSetModel(
	requestID string,
	requestData map[string]any,
) error {
	var modelPtr *string

	switch v := requestData["model"].(type) {
	case nil:
		modelPtr = nil
	case string:
		model := v
		modelPtr = &model
	default:
		a.injectErrorControlResponse(
			requestID,
			fmt.Errorf("%w: model must be a string or null", sdkerrors.ErrUnsupportedOption).Error(),
		)

		return nil
	}

	a.mu.Lock()
	a.modelOverride = modelPtr
	a.mu.Unlock()

	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "success",
		"request_id": requestID,
		"response":   map[string]any{},
	})

	return nil
}

func (a *AppServerAdapter) handleMCPStatus(ctx context.Context, requestID string) error {
	resp, err := a.inner.SendRequest(ctx, "mcpServerStatus/list", map[string]any{})
	if err != nil {
		a.injectErrorControlResponse(requestID, fmt.Sprintf("mcpServerStatus/list RPC: %v", err))

		return nil
	}

	payload := map[string]any{
		"mcpServers": []map[string]any{},
	}

	if resp.Result != nil {
		var result map[string]any
		if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr == nil {
			if data, ok := result["data"].([]any); ok && len(data) > 0 {
				servers := make([]map[string]any, 0, len(data))
				for _, raw := range data {
					entry, ok := raw.(map[string]any)
					if !ok {
						continue
					}

					name, _ := entry["name"].(string)
					authStatus, _ := entry["authStatus"].(string)

					if name == "" {
						continue
					}

					server := map[string]any{
						"name":       name,
						"status":     mapMCPAuthStatus(authStatus),
						"authStatus": authStatus,
					}

					if tools, ok := entry["tools"]; ok {
						server["tools"] = tools
					}

					if resources, ok := entry["resources"]; ok {
						server["resources"] = resources
					}

					if resourceTemplates, ok := entry["resourceTemplates"]; ok {
						server["resourceTemplates"] = resourceTemplates
					}

					servers = append(servers, server)
				}

				payload["mcpServers"] = servers
			}
		}
	}

	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "success",
		"request_id": requestID,
		"response":   payload,
	})

	return nil
}

func (a *AppServerAdapter) handleListModels(ctx context.Context, requestID string) error {
	result, err := a.listAllModels(ctx)
	if err != nil {
		a.injectErrorControlResponse(requestID, fmt.Sprintf("model/list RPC: %v", err))

		return nil
	}

	payload := map[string]any{
		"models": result.models,
	}

	if len(result.metadata) > 0 {
		payload["metadata"] = result.metadata
	}

	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "success",
		"request_id": requestID,
		"response":   payload,
	})

	return nil
}

type modelListResult struct {
	models   []map[string]any
	metadata map[string]any
}

type modelSpec struct {
	ContextWindow   int
	MaxOutputTokens int
}

const (
	modelMetadataSourceCLI      = "cli"
	modelMetadataSourceOfficial = "official"
	modelMetadataSourceRuntime  = "runtime"
)

var officialModelSpecs = map[string]modelSpec{
	"gpt-5.1-codex": {
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	},
	"gpt-5.1-codex-max": {
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	},
	"gpt-5.1-codex-mini": {
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	},
	"gpt-5.2": {
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	},
	"gpt-5.2-codex": {
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	},
	"gpt-5.3-codex": {
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	},
	"gpt-5.4": {
		ContextWindow:   1050000,
		MaxOutputTokens: 128000,
	},
}

var runtimeModelSpecs = map[string]modelSpec{
	"gpt-5.1-codex-max": {
		ContextWindow: 258400,
	},
	"gpt-5.1-codex-mini": {
		ContextWindow: 258400,
	},
	"gpt-5.2": {
		ContextWindow: 258400,
	},
	"gpt-5.2-codex": {
		ContextWindow: 258400,
	},
	"gpt-5.3-codex": {
		ContextWindow: 258400,
	},
	"gpt-5.3-codex-spark": {
		ContextWindow: 121600,
	},
	"gpt-5.4": {
		ContextWindow: 258400,
	},
}

func (a *AppServerAdapter) listAllModels(ctx context.Context) (*modelListResult, error) {
	var (
		cursor   string
		models   []map[string]any
		metadata map[string]any
	)

	for {
		params := map[string]any{
			"includeHidden": true,
			"limit":         100,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}

		resp, err := a.inner.SendRequest(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}

		pageModels, pageMetadata, nextCursor, err := parseModelListPage(resp.Result)
		if err != nil {
			return nil, err
		}

		models = append(models, pageModels...)

		if len(pageMetadata) > 0 {
			if metadata == nil {
				metadata = make(map[string]any, len(pageMetadata))
			}

			for key, value := range pageMetadata {
				metadata[key] = value
			}
		}

		if nextCursor == "" {
			break
		}

		cursor = nextCursor
	}

	return &modelListResult{
		models:   models,
		metadata: metadata,
	}, nil
}

func parseModelListPage(raw json.RawMessage) ([]map[string]any, map[string]any, string, error) {
	if len(raw) == 0 {
		return nil, nil, "", nil
	}

	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, "", err
	}

	var metadata map[string]any

	for key, value := range result {
		switch key {
		case "data", "nextCursor":
			continue
		default:
			if value == nil {
				continue
			}

			if metadata == nil {
				metadata = make(map[string]any)
			}

			metadata[key] = value
		}
	}

	var models []map[string]any
	if data, ok := result["data"].([]any); ok && len(data) > 0 {
		models = make([]map[string]any, 0, len(data))
		for _, rawEntry := range data {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				continue
			}

			model := normalizeModelEntry(entry)
			if model == nil {
				continue
			}

			models = append(models, model)
		}
	}

	nextCursor, _ := result["nextCursor"].(string)

	return models, metadata, nextCursor, nil
}

func normalizeModelEntry(entry map[string]any) map[string]any {
	id, _ := entry["id"].(string)
	if id == "" {
		return nil
	}

	model := map[string]any{
		"id":                  id,
		"supportsPersonality": false,
	}
	metadata := make(map[string]any)

	for key, value := range entry {
		switch key {
		case "id":
			model["id"] = value
		case "model":
			if v, ok := value.(string); ok {
				model["model"] = v
			}
		case "displayName":
			if v, ok := value.(string); ok {
				model["displayName"] = v
			}
		case "description":
			if v, ok := value.(string); ok {
				model["description"] = v
			}
		case "isDefault":
			if v, ok := value.(bool); ok {
				model["isDefault"] = v
			}
		case "hidden":
			if v, ok := value.(bool); ok {
				model["hidden"] = v
			}
		case "defaultReasoningEffort":
			if v, ok := value.(string); ok {
				model["defaultReasoningEffort"] = v
			}
		case "supportedReasoningEfforts":
			if v, ok := value.([]any); ok {
				model["supportedReasoningEfforts"] = v
			}
		case "inputModalities":
			if v, ok := value.([]any); ok {
				model["inputModalities"] = v
			}
		case "supportsPersonality":
			if v, ok := value.(bool); ok {
				model["supportsPersonality"] = v
			}
		default:
			if value == nil {
				continue
			}

			metadata[key] = value
		}
	}

	if len(metadata) > 0 {
		model["metadata"] = metadata
	}

	decorateModelMetadata(model)

	return model
}

func decorateModelMetadata(model map[string]any) {
	modelID, _ := model["id"].(string)
	modelName, _ := model["model"].(string)

	metadata, _ := model["metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any)
	}

	if _, exists := metadata["modelContextWindow"]; exists {
		setMetadataSourceIfMissing(metadata, "modelContextWindowSource", modelMetadataSourceCLI)
	} else {
		if spec := lookupModelSpec(modelID, modelName, officialModelSpecs); spec.ContextWindow != 0 {
			metadata["modelContextWindow"] = spec.ContextWindow
			metadata["modelContextWindowSource"] = modelMetadataSourceOfficial
		} else if spec := lookupModelSpec(modelID, modelName, runtimeModelSpecs); spec.ContextWindow != 0 {
			metadata["modelContextWindow"] = spec.ContextWindow
			metadata["modelContextWindowSource"] = modelMetadataSourceRuntime
		}
	}

	if _, exists := metadata["maxOutputTokens"]; exists {
		setMetadataSourceIfMissing(metadata, "maxOutputTokensSource", modelMetadataSourceCLI)
	} else if spec := lookupModelSpec(modelID, modelName, officialModelSpecs); spec.MaxOutputTokens != 0 {
		metadata["maxOutputTokens"] = spec.MaxOutputTokens
		metadata["maxOutputTokensSource"] = modelMetadataSourceOfficial
	}

	if len(metadata) == 0 {
		return
	}

	model["metadata"] = metadata
}

func setMetadataSourceIfMissing(metadata map[string]any, key string, value string) {
	if _, exists := metadata[key]; exists {
		return
	}

	metadata[key] = value
}

func lookupModelSpec(modelID string, modelName string, values map[string]modelSpec) modelSpec {
	if v, ok := values[modelID]; ok {
		return v
	}

	if v, ok := values[modelName]; ok {
		return v
	}

	return modelSpec{}
}

func (a *AppServerAdapter) handleRewindFiles(requestID string) error {
	a.injectErrorControlResponse(
		requestID,
		fmt.Errorf(
			"%w: rewind_files is not supported by codex app-server",
			sdkerrors.ErrUnsupportedControlRequest,
		).Error(),
	)

	return nil
}

func mapMCPAuthStatus(authStatus string) string {
	switch authStatus {
	case "oAuth", "bearerToken":
		return "connected"
	case "notLoggedIn":
		return "not_logged_in"
	case "unsupported":
		return "unsupported"
	default:
		if authStatus == "" {
			return "unknown"
		}

		return authStatus
	}
}

// handleControlResponse translates an outgoing control_response (from
// Controller to server) back into a JSON-RPC response for the inner
// transport. This handles hook/MCP callback responses.
func (a *AppServerAdapter) handleControlResponse(raw map[string]any) error {
	responseData, _ := raw["response"].(map[string]any)
	if responseData == nil {
		return nil
	}

	requestID, _ := responseData["request_id"].(string)
	if requestID == "" {
		return nil
	}

	a.mu.Lock()
	rpcID, ok := a.pendingRPCRequests[requestID]

	if ok {
		delete(a.pendingRPCRequests, requestID)
	}

	a.mu.Unlock()

	if !ok {
		return nil
	}

	payload, _ := responseData["response"].(map[string]any)

	var result json.RawMessage

	var rpcErr *RPCError

	subtype, _ := responseData["subtype"].(string)
	if subtype == "error" {
		errMsg, _ := responseData["error"].(string)
		rpcErr = &RPCError{Code: -32603, Message: errMsg}
	} else if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal response payload: %w", err)
		}

		result = data
	} else {
		result = json.RawMessage(`{}`)
	}

	return a.inner.SendResponse(rpcID, result, rpcErr)
}

// handleUserMessage translates an outgoing user message into a turn/start
// JSON-RPC call.
func (a *AppServerAdapter) handleUserMessage(
	ctx context.Context,
	raw map[string]any,
) error {
	messageData, _ := raw["message"].(map[string]any)

	// Build input as an array of content blocks — the app-server expects
	// a sequence, not a plain string.
	var input any

	if messageData != nil {
		if content, ok := messageData["content"].(string); ok {
			input = []map[string]any{
				{"type": "text", "text": content},
			}
		} else if contentBlocks, ok := messageData["content"].([]any); ok {
			input = contentBlocks
		}
	}

	a.mu.Lock()
	threadID := a.threadID
	modelOverride := a.modelOverride
	approvalPolicyOverride := a.approvalPolicyOverride
	sandboxPolicyOverride := cloneAnyMap(a.sandboxPolicyOverride)
	effortOverride := a.effortOverride
	outputSchemaOverride := cloneAnyValue(a.outputSchemaOverride)
	collaborationModeOverride := cloneAnyMap(a.collaborationModeOverride)
	a.mu.Unlock()

	outputSchema := outputSchemaOverride

	if rawOutputSchema, ok := raw["outputSchema"]; ok {
		normalizedOutputSchema, err := normalizeOutputSchema(rawOutputSchema)
		if err != nil {
			return err
		}

		outputSchema = normalizedOutputSchema
	}

	params := map[string]any{
		"input": input,
	}

	if threadID != "" {
		params["threadId"] = threadID
	}

	if modelOverride != nil {
		params["model"] = *modelOverride
	}

	if approvalPolicyOverride != nil {
		params["approvalPolicy"] = *approvalPolicyOverride
	}

	if sandboxPolicyOverride != nil {
		params["sandboxPolicy"] = sandboxPolicyOverride
	}

	if effortOverride != nil {
		params["effort"] = *effortOverride
	}

	if outputSchema != nil {
		params["outputSchema"] = outputSchema
	}

	a.mu.Lock()
	a.currentTurnHasOutputSchema = outputSchema != nil
	a.mu.Unlock()

	if collaborationModeOverride != nil {
		// Ensure the collaboration mode settings include a model — the CLI
		// requires it. Fall back to the model from params or the override.
		if settings, ok := collaborationModeOverride["settings"].(map[string]any); ok {
			if _, hasModel := settings["model"]; !hasModel {
				if m, ok := params["model"].(string); ok && m != "" {
					settings["model"] = m
				}
			}
		}

		params["collaborationMode"] = collaborationModeOverride
	}

	resp, err := a.inner.SendRequest(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("turn/start RPC: %w", err)
	}

	if resp.Result != nil {
		var result map[string]any
		if unmarshalErr := json.Unmarshal(resp.Result, &result); unmarshalErr == nil {
			if tid, ok := result["turnId"].(string); ok && tid != "" {
				a.mu.Lock()
				a.turnID = tid
				a.turnHasOutputSchema[tid] = outputSchema != nil
				a.mu.Unlock()
			} else if turnObj, ok := result["turn"].(map[string]any); ok {
				if tid, ok := turnObj["id"].(string); ok && tid != "" {
					a.mu.Lock()
					a.turnID = tid
					a.turnHasOutputSchema[tid] = outputSchema != nil
					a.mu.Unlock()
				}
			}
		}
	}

	return nil
}

// readLoop reads notifications and requests from the inner transport and
// translates them into exec-event format messages.
func (a *AppServerAdapter) readLoop() {
	defer a.wg.Done()
	defer func() {
		a.messagesClosed.Store(true)
		close(a.messages)
	}()
	defer close(a.errs)

	notifications := a.inner.Notifications()
	requests := a.inner.Requests()

	for {
		select {
		case notif, ok := <-notifications:
			if !ok {
				notifications = nil

				if requests == nil {
					return
				}

				continue
			}

			a.handleNotification(notif)

		case req, ok := <-requests:
			if !ok {
				requests = nil

				if notifications == nil {
					return
				}

				continue
			}

			a.handleServerRequest(req)

		case <-a.done:
			return
		}
	}
}

// handleNotification translates a JSON-RPC notification into exec-event
// format and sends it to the messages channel.
func (a *AppServerAdapter) handleNotification(notif *RPCNotification) {
	event := a.translateNotification(notif)
	if event == nil {
		return
	}

	select {
	case a.messages <- event:
	case <-a.done:
	}
}

// handleServerRequest translates an incoming JSON-RPC request from the
// server into a control_request message for the Controller to handle.
func (a *AppServerAdapter) handleServerRequest(req *RPCIncomingRequest) {
	syntheticID := fmt.Sprintf("rpc_%d", req.ID)

	a.mu.Lock()
	a.pendingRPCRequests[syntheticID] = req.ID
	sdkServerNames := cloneStringSet(a.sdkMCPServerNames)
	a.mu.Unlock()

	var requestPayload map[string]any
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &requestPayload); err != nil {
			requestPayload = make(map[string]any, 1)
		}
	} else {
		requestPayload = make(map[string]any, 1)
	}

	if toolName, ok := requestPayload["tool"].(string); ok && toolName != "" {
		requestPayload["tool"] = internalToPublicToolName(toolName, sdkServerNames)
	}

	if toolName, ok := requestPayload["tool_name"].(string); ok && toolName != "" {
		requestPayload["tool_name"] = internalToPublicToolName(toolName, sdkServerNames)
	}

	requestPayload["subtype"] = methodToSubtype(req.Method)

	msg := map[string]any{
		"type":       "control_request",
		"request_id": syntheticID,
		"request":    requestPayload,
	}

	select {
	case a.messages <- msg:
	case <-a.done:
	}
}

// translateNotification converts a JSON-RPC notification into an exec-event
// format map that message.Parse() can handle. Every notification produces an
// event; nothing is silently dropped.
func (a *AppServerAdapter) translateNotification(
	notif *RPCNotification,
) map[string]any {
	var params map[string]any
	if notif.Params != nil {
		if err := json.Unmarshal(notif.Params, &params); err != nil {
			a.log.Warn("failed to unmarshal notification params",
				slog.String("method", notif.Method),
				slog.String("error", err.Error()),
			)

			params = make(map[string]any, 1)
		}
	} else {
		params = make(map[string]any, 1)
	}

	switch notif.Method {
	case "thread/started":
		event := map[string]any{"type": "thread.started"}
		if tid := extractThreadID(params); tid != "" {
			event["thread_id"] = tid
		}

		a.mu.Lock()
		if tid, _ := event["thread_id"].(string); tid != "" {
			a.threadID = tid
		} else if a.threadID != "" {
			event["thread_id"] = a.threadID
		}
		a.mu.Unlock()

		return event

	case "turn/started":
		if turnObj, ok := params["turn"].(map[string]any); ok {
			if tid, ok := turnObj["id"].(string); ok && tid != "" {
				a.mu.Lock()
				a.turnID = tid
				a.lastAssistantText = ""

				a.lastAssistantTextByTurn[tid] = ""
				if _, known := a.turnHasOutputSchema[tid]; !known {
					a.turnHasOutputSchema[tid] = a.currentTurnHasOutputSchema
				}
				a.mu.Unlock()
			}
		}

		return map[string]any{"type": "turn.started"}

	case "item/started":
		return a.translateItemNotification("item.started", params)

	case "item/agentMessage/delta":
		return a.translateTextDeltaNotification(params)

	case "item/reasoning/textDelta":
		return a.translateTextDeltaNotification(params)

	case "item/reasoning/summaryTextDelta":
		return a.translateTextDeltaNotification(params)

	case "item/commandExecution/outputDelta":
		return a.translateTextDeltaNotification(params)

	case "item/fileChange/outputDelta":
		return a.translateTextDeltaNotification(params)

	case "item/plan/delta":
		return a.translateTextDeltaNotification(params)

	case "item/completed":
		return a.translateItemNotification("item.completed", params)

	case "turn/completed":
		return a.translateTurnCompleted(params)

	case "turn/failed":
		event := map[string]any{"type": "turn.failed"}

		turnID := extractTurnID(params)
		if turnID != "" {
			a.mu.Lock()
			delete(a.turnHasOutputSchema, turnID)
			delete(a.lastAssistantTextByTurn, turnID)
			a.mu.Unlock()
		}

		if errMsg, ok := params["error"].(string); ok {
			event["error"] = map[string]any{"message": errMsg}
		} else if errObj, ok := params["error"].(map[string]any); ok {
			event["error"] = errObj
		}

		return event

	case "thread/tokenUsage/updated":
		return a.translateTokenUsageUpdated(params)

	case "account/rateLimits/updated":
		return map[string]any{
			"type":    "system",
			"subtype": "account.rate_limits.updated",
			"data":    params,
		}

	case "error":
		return a.translateErrorNotification(params)

	default:
		// Handle codex/event/* namespace.
		if strings.HasPrefix(notif.Method, "codex/event/") {
			return a.translateCodexEvent(notif.Method, params)
		}

		// Pass through all unknown notifications as system messages.
		a.log.Debug("passing through unknown notification",
			slog.String("method", notif.Method),
		)

		return map[string]any{
			"type":    "system",
			"subtype": notif.Method,
			"data":    params,
		}
	}
}

// translateErrorNotification converts an "error" notification into the
// exec-event "error" format that message.Parse handles as an AssistantMessage
// with an error type.
func (a *AppServerAdapter) translateErrorNotification(params map[string]any) map[string]any {
	var msg string

	if errObj, ok := params["error"].(map[string]any); ok {
		msg, _ = errObj["message"].(string)
	}

	if msg == "" {
		msg, _ = params["message"].(string)
	}

	if msg == "" {
		msg = "unknown error"
	}

	return map[string]any{
		"type":    "error",
		"message": msg,
	}
}

// translateItemNotification handles item/started and item/completed,
// dispatching userMessage items to the user message format and all
// other item types through the standard item translation.
func (a *AppServerAdapter) translateItemNotification(
	eventType string,
	params map[string]any,
) map[string]any {
	nested, _ := params["item"].(map[string]any)
	if nested == nil {
		nested = params
	}

	itemType, _ := nested["type"].(string)

	// userMessage items need special handling: item/started emits a
	// "user" message so the content is visible; item/completed emits a
	// system lifecycle event (no new content to show).
	if itemType == "userMessage" {
		if eventType == "item.started" {
			return a.translateUserMessageItem(nested)
		}

		return map[string]any{
			"type":    "system",
			"subtype": "user_message.completed",
			"data":    nested,
		}
	}

	item := a.extractAndTranslateItem(params)

	if eventType == "item.completed" {
		if itemType, ok := item["type"].(string); ok && itemType == "agent_message" {
			if text, ok := item["text"].(string); ok && strings.TrimSpace(text) != "" {
				turnID := extractTurnID(params)

				a.mu.Lock()
				if turnID == "" {
					turnID = a.turnID
				}

				a.lastAssistantText = text
				if turnID != "" {
					a.lastAssistantTextByTurn[turnID] = text
				}
				a.mu.Unlock()
			}
		}
	}

	return map[string]any{
		"type": eventType,
		"item": item,
	}
}

// translateUserMessageItem converts a userMessage item into the "user"
// message format that parseUserMessage already handles.
func (a *AppServerAdapter) translateUserMessageItem(
	nested map[string]any,
) map[string]any {
	if contentArr, ok := nested["content"].([]any); ok {
		textOnly := true
		parts := make([]string, 0, len(contentArr))

		for _, block := range contentArr {
			blockMap, ok := block.(map[string]any)
			if !ok {
				textOnly = false

				break
			}

			if blockMap["type"] != "text" {
				textOnly = false

				break
			}

			text, ok := blockMap["text"].(string)
			if !ok {
				textOnly = false

				break
			}

			parts = append(parts, text)
		}

		content := cloneAnyValue(contentArr)
		if textOnly {
			content = strings.Join(parts, "\n")
		}

		event := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": content,
			},
		}

		if id, ok := nested["id"].(string); ok {
			event["uuid"] = id
		}

		return event
	}

	event := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "",
		},
	}

	if id, ok := nested["id"].(string); ok {
		event["uuid"] = id
	}

	return event
}

func (a *AppServerAdapter) translateTextDeltaNotification(
	params map[string]any,
) map[string]any {
	if !a.includePartialMessages {
		return nil
	}

	delta, _ := params["delta"].(string)
	itemID, _ := params["itemId"].(string)

	a.mu.Lock()
	sessionID := a.threadID
	a.mu.Unlock()

	if threadID, ok := params["threadId"].(string); ok && threadID != "" {
		sessionID = threadID
	}

	return map[string]any{
		"type":       "stream_event",
		"uuid":       itemID,
		"session_id": sessionID,
		"event": map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{
				"type": "text_delta",
				"text": delta,
			},
		},
	}
}

// translateTurnCompleted builds a turn.completed event, injecting cached
// token usage when no inline usage is present.
func (a *AppServerAdapter) translateTurnCompleted(
	params map[string]any,
) map[string]any {
	event := map[string]any{"type": "turn.completed"}

	a.mu.Lock()
	if a.threadID != "" {
		event["session_id"] = a.threadID
		event["thread_id"] = a.threadID
	}
	a.mu.Unlock()

	if usage, ok := params["usage"].(map[string]any); ok {
		event["usage"] = usage
	} else {
		// No inline usage — try cached token usage from
		// thread/tokenUsage/updated.
		a.mu.Lock()
		cached := a.lastTokenUsage
		a.mu.Unlock()

		if cached != nil {
			event["usage"] = convertTokenUsage(cached)
		}
	}

	if v, ok := params["isError"]; ok {
		event["is_error"] = v
	}

	turnID := extractTurnID(params)

	a.mu.Lock()
	hasOutputSchema := a.currentTurnHasOutputSchema
	lastAssistantText := a.lastAssistantText

	if turnID != "" {
		if turnScoped, ok := a.turnHasOutputSchema[turnID]; ok {
			hasOutputSchema = turnScoped

			delete(a.turnHasOutputSchema, turnID)
		}

		if turnText, ok := a.lastAssistantTextByTurn[turnID]; ok {
			lastAssistantText = turnText

			delete(a.lastAssistantTextByTurn, turnID)
		} else {
			lastAssistantText = ""
		}
	}
	a.mu.Unlock()

	if v, ok := params["result"]; ok {
		event["result"] = v

		if hasOutputSchema {
			if structured, ok := parseStructuredOutputValue(v); ok {
				event["structured_output"] = structured
			}
		}
	} else {
		if strings.TrimSpace(lastAssistantText) != "" {
			event["result"] = lastAssistantText

			if hasOutputSchema {
				if structured, ok := parseStructuredOutputValue(lastAssistantText); ok {
					event["structured_output"] = structured
				}
			}
		}
	}

	return event
}

// translateTokenUsageUpdated caches the latest token usage and emits a
// system message.
func (a *AppServerAdapter) translateTokenUsageUpdated(
	params map[string]any,
) map[string]any {
	if tokenUsage, ok := params["tokenUsage"].(map[string]any); ok {
		a.mu.Lock()
		a.lastTokenUsage = tokenUsage
		a.mu.Unlock()
	}

	return map[string]any{
		"type":    "system",
		"subtype": "thread.token_usage.updated",
		"data":    params,
	}
}

// convertTokenUsage extracts the "last" usage object from cached token
// usage and converts camelCase keys to snake_case for the exec-event format.
func convertTokenUsage(tokenUsage map[string]any) map[string]any {
	last, ok := tokenUsage["last"].(map[string]any)
	if !ok {
		return nil
	}

	keyMap := map[string]string{ //nolint:gosec // G101 false positive: field name mapping, not credentials
		"totalTokens":           "total_tokens",
		"inputTokens":           "input_tokens",
		"outputTokens":          "output_tokens",
		"cachedInputTokens":     "cached_input_tokens",
		"reasoningOutputTokens": "reasoning_output_tokens",
	}

	result := make(map[string]any, len(last))

	for k, v := range last {
		if snakeKey, ok := keyMap[k]; ok {
			result[snakeKey] = v
		} else {
			result[k] = v
		}
	}

	return result
}

func extractTurnID(params map[string]any) string {
	if turnID, ok := params["turnId"].(string); ok && turnID != "" {
		return turnID
	}

	if turnID, ok := params["turn_id"].(string); ok && turnID != "" {
		return turnID
	}

	if turnObj, ok := params["turn"].(map[string]any); ok {
		if turnID, ok := turnObj["id"].(string); ok && turnID != "" {
			return turnID
		}
	}

	nested, _ := params["item"].(map[string]any)
	if nested == nil {
		nested = params
	}

	if turnID, ok := nested["turnId"].(string); ok && turnID != "" {
		return turnID
	}

	if turnID, ok := nested["turn_id"].(string); ok && turnID != "" {
		return turnID
	}

	if turnObj, ok := nested["turn"].(map[string]any); ok {
		if turnID, ok := turnObj["id"].(string); ok && turnID != "" {
			return turnID
		}
	}

	return ""
}

const permissionModePlan = "plan"

func permissionModeToTurnOverrides(mode string) (string, map[string]any, error) {
	const (
		approvalOnRequest = "on-request"
		approvalNever     = "never"
	)

	switch mode {
	case "", "default", permissionModePlan:
		return approvalOnRequest, nil, nil
	case "acceptEdits":
		return approvalOnRequest, map[string]any{"type": "workspaceWrite"}, nil
	case "bypassPermissions":
		return approvalNever, map[string]any{"type": "dangerFullAccess"}, nil
	default:
		return "", nil, fmt.Errorf("%w: permission mode %q", sdkerrors.ErrUnsupportedOption, mode)
	}
}

func normalizeApprovalPolicy(value string) (string, error) {
	switch value {
	case "on-request":
		return "on-request", nil
	case "on-failure":
		return "on-failure", nil
	case "untrusted":
		return "untrusted", nil
	case "never":
		return "never", nil
	case "":
		return "", nil
	default:
		return "", fmt.Errorf("%w: approvalPolicy %q", sdkerrors.ErrUnsupportedOption, value)
	}
}

func normalizeSandboxMode(value string) (string, error) {
	switch value {
	case "read-only", "workspace-write", "danger-full-access":
		return value, nil
	case "":
		return "", nil
	default:
		return "", fmt.Errorf("%w: sandbox %q", sdkerrors.ErrUnsupportedOption, value)
	}
}

func normalizeEffort(value string) (string, error) {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return value, nil
	default:
		return "", fmt.Errorf("%w: reasoningEffort %q", sdkerrors.ErrUnsupportedOption, value)
	}
}

func parseStructuredOutputValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, false
		}

		var structured any
		if err := json.Unmarshal([]byte(v), &structured); err != nil {
			return nil, false
		}

		return structured, true
	case map[string]any, []any:
		return cloneAnyValue(v), true
	default:
		return nil, false
	}
}

func normalizeOutputSchema(value any) (any, error) {
	normalizeParsed := func(parsed any) any {
		m, ok := parsed.(map[string]any)
		if !ok {
			return parsed
		}

		formatType, _ := m["type"].(string)
		if formatType == "json_schema" {
			if inner, ok := m["schema"]; ok {
				cloned := cloneAnyValue(inner)
				if cm, isMap := cloned.(map[string]any); isMap {
					schema.EnforceAdditionalProperties(cm)
				}

				return cloned
			}
		}

		schema.EnforceAdditionalProperties(m)

		return parsed
	}

	switch v := value.(type) {
	case nil:
		return nil, nil //nolint:nilnil // nil output schema is the explicit "unset" signal.
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, nil //nolint:nilnil // empty schema string is treated as unset.
		}

		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return nil, fmt.Errorf(
				"%w: outputSchema must be valid JSON schema: %v",
				sdkerrors.ErrUnsupportedOption,
				err,
			)
		}

		return normalizeParsed(parsed), nil
	default:
		return normalizeParsed(cloneAnyValue(value)), nil
	}
}

// buildCollaborationMode constructs the collaborationMode object for turn/start.
// The Codex CLI requires this on each turn/start when plan mode is active —
// ModeKind::Plan enables request_user_input, while ModeKind::Default does not.
func buildCollaborationMode(mode string, model string) map[string]any {
	settings := map[string]any{
		"developerInstructions": nil,
	}

	if model != "" {
		settings["model"] = model
	}

	return map[string]any{
		"mode":     mode,
		"settings": settings,
	}
}

func cloneAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}

	dst := make(map[string]any, len(src))
	maps.Copy(dst, src)

	return dst
}

func cloneMapSlice(src []map[string]any) []map[string]any {
	if src == nil {
		return nil
	}

	dst := make([]map[string]any, len(src))
	for i, item := range src {
		dst[i] = cloneAnyMap(item)
	}

	return dst
}

func cloneStringSet(src map[string]struct{}) map[string]struct{} {
	if src == nil {
		return nil
	}

	return maps.Clone(src)
}

func cloneAnyValue(src any) any {
	switch v := src.(type) {
	case nil:
		return nil
	case map[string]any:
		dst := make(map[string]any, len(v))
		for k, item := range v {
			dst[k] = cloneAnyValue(item)
		}

		return dst
	case []any:
		dst := make([]any, len(v))
		for i, item := range v {
			dst[i] = cloneAnyValue(item)
		}

		return dst
	default:
		return v
	}
}

// codexEventDuplicates lists codex/event/* methods that duplicate
// notifications already handled via the standard JSON-RPC protocol.
var codexEventDuplicates = map[string]bool{
	"codex/event/item_started":                true,
	"codex/event/item_completed":              true,
	"codex/event/agent_message_content_delta": true,
	"codex/event/agent_message_delta":         true,
	"codex/event/agent_message":               true,
	"codex/event/user_message":                true,
}

// codexEventSubtypes maps unique codex/event/* methods to their system
// message subtypes.
var codexEventSubtypes = map[string]string{
	"codex/event/task_started":         "task.started",
	"codex/event/task_complete":        "task.complete",
	"codex/event/thread_rolled_back":   "thread.rolled_back",
	"codex/event/token_count":          "token.count",
	"codex/event/mcp_startup_update":   "mcp.startup_update",
	"codex/event/mcp_startup_complete": "mcp.startup_complete",
}

func (a *AppServerAdapter) normalizeTypedCodexEventData(
	method string,
	params map[string]any,
) map[string]any {
	data := cloneAnyMap(params)

	switch method {
	case "codex/event/task_started":
		turnID, _ := data["turn_id"].(string)
		if turnID == "" {
			turnID, _ = data["turnId"].(string)
		}

		if turnID != "" {
			data["turn_id"] = turnID
			delete(data, "turnId")

			a.mu.Lock()
			a.turnID = turnID
			a.mu.Unlock()
		} else {
			a.mu.Lock()
			if a.turnID != "" {
				data["turn_id"] = a.turnID
			}
			a.mu.Unlock()
		}

		if mode, ok := data["collaborationModeKind"]; ok {
			data["collaboration_mode_kind"] = mode
			delete(data, "collaborationModeKind")
		}

		if window, ok := data["modelContextWindow"]; ok {
			data["model_context_window"] = window
			delete(data, "modelContextWindow")
		}

	case "codex/event/task_complete":
		turnID, _ := data["turn_id"].(string)
		if turnID == "" {
			turnID, _ = data["turnId"].(string)
		}

		if turnID != "" {
			data["turn_id"] = turnID
			delete(data, "turnId")
		} else {
			a.mu.Lock()
			if a.turnID != "" {
				data["turn_id"] = a.turnID
			}
			a.mu.Unlock()
		}

		if lastAgentMessage, ok := data["lastAgentMessage"]; ok {
			data["last_agent_message"] = lastAgentMessage
			delete(data, "lastAgentMessage")
		}

	case "codex/event/thread_rolled_back":
		if numTurns, ok := data["numTurns"]; ok {
			data["num_turns"] = numTurns
			delete(data, "numTurns")
		}
	}

	return data
}

// translateCodexEvent handles codex/event/* notifications. Duplicates of
// standard protocol events are logged and dropped; unique events are
// emitted as system messages.
func (a *AppServerAdapter) translateCodexEvent(
	method string,
	params map[string]any,
) map[string]any {
	if codexEventDuplicates[method] {
		a.log.Debug("dropping duplicate codex event",
			slog.String("method", method),
		)

		return nil
	}

	if subtype, ok := codexEventSubtypes[method]; ok {
		return map[string]any{
			"type":    "system",
			"subtype": subtype,
			"data":    a.normalizeTypedCodexEventData(method, params),
		}
	}

	// Unknown codex/event/* — pass through with derived subtype.
	name := strings.TrimPrefix(method, "codex/event/")

	return map[string]any{
		"type":    "system",
		"subtype": "codex.event." + name,
		"data":    params,
	}
}

// extractAndTranslateItem extracts the nested "item" object from app-server
// notification params and converts camelCase types to snake_case. It also
// handles reasoning items whose text lives in a "summary" array.
func (a *AppServerAdapter) extractAndTranslateItem(params map[string]any) map[string]any {
	// App-server nests the item under an "item" key.
	nested, ok := params["item"].(map[string]any)
	if !ok {
		nested = params
	}

	item := make(map[string]any, len(nested))

	maps.Copy(item, nested)

	if itemType, ok := item["type"].(string); ok {
		item["type"] = camelToSnake(itemType)
	}

	a.mu.Lock()
	sdkServerNames := cloneStringSet(a.sdkMCPServerNames)
	a.mu.Unlock()

	if name, ok := item["name"].(string); ok && name != "" {
		item["name"] = internalToPublicToolName(name, sdkServerNames)
	}

	if tool, ok := item["tool"].(string); ok && tool != "" {
		item["tool"] = internalToPublicToolName(tool, sdkServerNames)
	}

	// Reasoning items carry text in a "summary" string array, not "text".
	switch item["type"] {
	case "reasoning":
		if summaryArr, ok := item["summary"].([]any); ok && len(summaryArr) > 0 {
			parts := make([]string, 0, len(summaryArr))

			for _, v := range summaryArr {
				if s, ok := v.(string); ok {
					parts = append(parts, s)
				}
			}

			if len(parts) > 0 {
				item["text"] = strings.Join(parts, "\n")
			}
		}
	case "image_view":
		if path, ok := item["path"].(string); ok && path != "" {
			item["text"] = fmt.Sprintf("Viewed image: %s", path)
		}
	case "entered_review_mode":
		if review, ok := item["review"].(string); ok && review != "" {
			item["text"] = fmt.Sprintf("Entered review mode: %s", review)
		}
	case "exited_review_mode":
		if review, ok := item["review"].(string); ok && review != "" {
			item["text"] = fmt.Sprintf("Exited review mode: %s", review)
		}
	case "context_compaction":
		item["text"] = "Context compacted."
	case "collab_agent_tool_call":
		item["text"] = summarizeCollabAgentToolCall(item)
	}

	return item
}

// injectControlResponse fabricates a control_response and injects it into
// the messages channel for the Controller to pick up.
func (a *AppServerAdapter) injectControlResponse(
	requestID string,
	responseData map[string]any,
) {
	// Guard against sending on a closed channel. The readLoop goroutine
	// closes a.messages when the inner transport shuts down, which can
	// race with control-request handlers still running on other goroutines.
	// The atomic check handles the common case; the recover covers the
	// narrow window between the check and the send.
	if a.messagesClosed.Load() {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			a.log.Debug("inject suppressed: messages channel closed during shutdown")
		}
	}()

	msg := map[string]any{
		"type":     "control_response",
		"response": responseData,
	}

	select {
	case a.messages <- msg:
	case <-a.done:
	}
}

func (a *AppServerAdapter) injectErrorControlResponse(requestID string, errMsg string) {
	a.injectControlResponse(requestID, map[string]any{
		"subtype":    "error",
		"request_id": requestID,
		"error":      errMsg,
	})
}

// extractThreadID extracts the thread ID from a thread/start response.
func extractThreadID(result map[string]any) string {
	if thread, ok := result["thread"].(map[string]any); ok {
		if id, ok := thread["id"].(string); ok {
			return id
		}
	}

	return ""
}

// camelToSnake converts a camelCase string to snake_case.
// Specifically handles the known item types from the app-server protocol.
var camelToSnakeMap = map[string]string{
	"agentMessage":        "agent_message",
	"collabAgentToolCall": "collab_agent_tool_call",
	"commandExecution":    "command_execution",
	"contextCompaction":   "context_compaction",
	"dynamicToolCall":     "dynamic_tool_call",
	"enteredReviewMode":   "entered_review_mode",
	"exitedReviewMode":    "exited_review_mode",
	"fileChange":          "file_change",
	"imageView":           "image_view",
	"mcpToolCall":         "mcp_tool_call",
	"plan":                "plan",
	"userMessage":         "user_message",
	"webSearch":           "web_search",
	"todoList":            "todo_list",
	"reasoning":           "reasoning",
	"error":               "error",
	"toolSearchCall":      "tool_search_call",
	"imageGenerationCall": "image_generation_call",
	"customToolCall":      "custom_tool_call",
}

func camelToSnake(s string) string {
	if mapped, ok := camelToSnakeMap[s]; ok {
		return mapped
	}

	return s
}

// methodToSubtype maps a JSON-RPC method name to a control_request subtype.
func methodToSubtype(method string) string {
	// Strip namespace prefix if present (e.g., "hooks/callback" -> "hook_callback")
	parts := strings.SplitN(method, "/", 2)
	if len(parts) == 2 {
		return parts[0] + "_" + parts[1]
	}

	return method
}

func summarizeCollabAgentToolCall(item map[string]any) string {
	tool, _ := item["tool"].(string)
	status, _ := item["status"].(string)
	prompt, _ := item["prompt"].(string)

	parts := make([]string, 0, 3)
	if tool != "" {
		parts = append(parts, "Collab tool "+tool)
	} else {
		parts = append(parts, "Collab tool call")
	}

	if status != "" {
		parts = append(parts, "status "+status)
	}

	if prompt != "" {
		parts = append(parts, prompt)
	}

	return strings.Join(parts, ": ")
}
