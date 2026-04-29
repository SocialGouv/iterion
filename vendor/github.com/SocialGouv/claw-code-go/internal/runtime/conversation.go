package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/hooks"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/apikit"
	clawctx "github.com/SocialGouv/claw-code-go/internal/context"
	lifehooks "github.com/SocialGouv/claw-code-go/internal/hooks"
	"github.com/SocialGouv/claw-code-go/internal/lsp"
	"github.com/SocialGouv/claw-code-go/internal/mcp"
	"github.com/SocialGouv/claw-code-go/internal/permissions"
	"github.com/SocialGouv/claw-code-go/internal/plugins"
	"github.com/SocialGouv/claw-code-go/internal/runtime/task"
	"github.com/SocialGouv/claw-code-go/internal/runtime/team"
	"github.com/SocialGouv/claw-code-go/internal/runtime/worker"
	"github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/internal/usage"
	"github.com/SocialGouv/claw-code-go/plugin"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

const systemPromptBase = `You are Claude Code, an AI assistant for software engineering tasks. You have access to tools for running bash commands, reading and writing files, searching with glob patterns, and grepping for patterns in code. Use these tools to help users with coding tasks.`

// ConversationLoop manages the agentic conversation loop with tool use.
type ConversationLoop struct {
	Client          api.APIClient // provider-agnostic client interface
	Session         *Session
	Tools           []api.Tool
	Permissions     *Permissions
	PermManager     *permissions.Manager // Phase 5 permission manager (may be nil)
	Config          *Config
	MCPRegistry     *mcp.Registry          // MCP server registry (may be nil)
	Compaction      CompactionState        // Phase 6 token tracking and compaction state
	CtxAssembler    *clawctx.Assembler     // Phase 12 context assembler (may be nil)
	Usage           *usage.Tracker         // Phase 13 per-session token usage tracker
	TelemetrySink   apikit.TelemetrySink   // Telemetry event sink (may be nil)
	Tracer          *apikit.SessionTracer  // Session telemetry tracer (may be nil)
	PluginRegistry  *plugin.PluginRegistry // Plugin registry (may be nil)
	MarketplaceMgr  *plugins.Manager       // Marketplace plugin manager (may be nil; wired by main when CLAW_MARKETPLACE_URL is set)
	HookRunner      *hooks.HookRunner      // Shell-command hook runner (may be nil; wired from config + plugin hooks)
	LifecycleHooks  *lifehooks.Runner      // In-process programmatic hooks (may be nil; default no-op)
	CommandRegistry interface{}            // Slash command registry (may be nil; *commands.Registry)

	// --- Batch 2: registries for CRUD tools ---
	TaskRegistry   *task.Registry         // Task registry (may be nil)
	TeamRegistry   *team.TeamRegistry     // Team registry (may be nil)
	CronRegistry   *team.CronRegistry     // Cron registry (may be nil)
	WorkerRegistry *worker.WorkerRegistry // Worker registry (may be nil)
	LspRegistry    *lsp.Registry          // LSP server registry (may be nil)
	McpAuthState   *mcp.AuthState         // MCP auth state tracker (may be nil)

	// PlanModeActive tracks whether plan mode is currently engaged.
	PlanModeActive bool

	// Asker is consulted by the non-streaming ExecuteTool path for ask_user.
	// May be nil (legacy fallback prints to stdout). The streaming path uses
	// the TurnEventAskUser channel and is unaffected by this field.
	Asker tools.Asker

	// toolCallCount is the number of tool_use blocks executed in this session.
	// Incremented atomically in ExecuteTool for goroutine safety (Rust parity:
	// pending_tool_use_count is tracked per-turn; we track cumulative for the
	// session to feed ToolCallCount() on the LoopAdapter).
	toolCallCount atomic.Int64

	// Build-time info passed through to LoopAdapter. Set by the caller.
	BuildVersion string
	BuildCommit  string
}

// ToolCallCount returns the total number of tool calls executed in this
// conversation loop (goroutine-safe).
func (loop *ConversationLoop) ToolCallCount() int {
	return int(loop.toolCallCount.Load())
}

// NewConversationLoop creates a new conversation loop with the given client.
// Use NewProviderClient to create an appropriate client for the configured provider.
func NewConversationLoop(cfg *Config, client api.APIClient) *ConversationLoop {
	workDir, _ := os.Getwd()
	return &ConversationLoop{
		Client:  client,
		Session: NewSession(),
		Tools: []api.Tool{
			// Original 10 tools
			tools.BashTool(),
			tools.ReadFileTool(),
			tools.WriteFileTool(),
			tools.GlobTool(),
			tools.GrepTool(),
			tools.FileEditTool(),
			tools.WebFetchTool(),
			tools.WebSearchTool(),
			tools.AskUserQuestionTool(),
			tools.TodoWriteTool(),
			// Batch 2: simple stateless tools
			tools.SleepTool(),
			tools.ConfigTool(),
			tools.REPLTool(),
			tools.NotebookEditTool(),
			tools.StructuredOutputTool(),
			tools.EnterPlanModeTool(),
			tools.ExitPlanModeTool(),
			tools.SendUserMessageTool(),
			// Batch 2: registry-backed CRUD tools
			tools.TaskCreateTool(),
			tools.TaskGetTool(),
			tools.TaskListTool(),
			tools.TaskOutputTool(),
			tools.TaskStopTool(),
			tools.TaskUpdateTool(),
			tools.RunTaskPacketTool(),
			tools.TeamCreateTool(),
			tools.TeamGetTool(),
			tools.TeamListTool(),
			tools.TeamDeleteTool(),
			tools.CronCreateTool(),
			tools.CronGetTool(),
			tools.CronListTool(),
			tools.CronDeleteTool(),
			tools.RemoteTriggerTool(),
			// Batch 2: orchestration tools
			tools.AgentTool(),
			tools.SkillTool(),
			tools.ToolSearchTool(),
			// Batch 3: worker tools
			tools.WorkerCreateTool(),
			tools.WorkerGetTool(),
			tools.WorkerObserveTool(),
			tools.WorkerResolveTrustTool(),
			tools.WorkerAwaitReadyTool(),
			tools.WorkerSendPromptTool(),
			tools.WorkerRestartTool(),
			tools.WorkerTerminateTool(),
			tools.WorkerObserveCompletionTool(),
			// Batch 3: LSP tool
			tools.LspTool(),
			// Batch 3: MCP resource/auth tools
			tools.ListMcpResourcesTool(),
			tools.ReadMcpResourceTool(),
			tools.McpAuthTool(),
		},
		Permissions:    DefaultPermissions(),
		Config:         cfg,
		CtxAssembler:   clawctx.NewAssembler(workDir),
		Usage:          usage.NewTracker(cfg.Model),
		TaskRegistry:   task.NewRegistry(),
		TeamRegistry:   team.NewTeamRegistry(),
		CronRegistry:   team.NewCronRegistry(),
		WorkerRegistry: worker.NewWorkerRegistry(),
	}
}

// currentPermMode returns the active permission mode for the conversation.
func (loop *ConversationLoop) currentPermMode() permissions.PermissionMode {
	if loop.PermManager != nil {
		return loop.PermManager.Mode
	}
	// Fall back to config string.
	mode, err := permissions.ParsePermissionMode(loop.Config.PermissionMode)
	if err != nil {
		return permissions.ModePrompt
	}
	return mode
}

// workspaceRoot returns the workspace root directory.
func (loop *ConversationLoop) workspaceRoot() string {
	if loop.CtxAssembler != nil {
		return loop.CtxAssembler.WorkDir
	}
	wd, _ := os.Getwd()
	return wd
}

// planModeStateDir returns the directory for plan mode state persistence.
// Returns empty string if no config directory is available.
func (loop *ConversationLoop) planModeStateDir() string {
	if loop.Config != nil && loop.Config.SessionDir != "" {
		return filepath.Dir(loop.Config.SessionDir) // e.g. .claude/
	}
	return ""
}

// SystemPrompt returns the rendered system prompt for diagnostic use.
func (loop *ConversationLoop) SystemPrompt() string {
	return loop.systemPrompt()
}

// systemPrompt returns the system prompt, optionally injecting project context,
// compaction summary, and MCP tool context.
func (loop *ConversationLoop) systemPrompt() string {
	var parts []string
	parts = append(parts, systemPromptBase)

	// Inject project context (Phase 12): environment, git status, CLAUDE.md.
	if loop.CtxAssembler != nil {
		if ctx := loop.CtxAssembler.Assemble(); ctx != "" {
			parts = append(parts, ctx)
		}
	}

	// Inject compaction summary when the session has one (Phase 6).
	if loop.Session != nil && loop.Session.CompactionSummary != "" {
		parts = append(parts, FormatCompactSummary(loop.Session.CompactionSummary))
	}

	// Append MCP tool list if any servers are connected.
	if loop.MCPRegistry != nil {
		mcpTools := loop.MCPRegistry.AllTools()
		if len(mcpTools) > 0 {
			names := make([]string, len(mcpTools))
			for i, t := range mcpTools {
				names[i] = t.Name
			}
			parts = append(parts, "Additional tools available via MCP: "+strings.Join(names, ", ")+".")
		}
	}

	return strings.Join(parts, "\n\n")
}

// isAnthropicProvider returns true when the configured provider is Anthropic
// (the default) or any Anthropic-compatible cloud provider (Bedrock, Vertex).
func (loop *ConversationLoop) isAnthropicProvider() bool {
	switch loop.Config.ProviderName {
	case "", "anthropic", "bedrock", "vertex":
		return true
	default:
		return false
	}
}

// injectCacheControl adds Anthropic prompt cache breakpoints to a request.
// For non-Anthropic providers this is a no-op.
//
// Strategy (mirrors the original Claude Code TypeScript):
//   - System prompt: convert System string to SystemBlocks with cache_control
//     on the last block.
//   - Tools: mark the last tool with cache_control.
func (loop *ConversationLoop) injectCacheControl(req *api.CreateMessageRequest) {
	if !loop.isAnthropicProvider() {
		return
	}

	if req.System != "" && len(req.SystemBlocks) == 0 {
		req.SystemBlocks = []api.ContentBlock{
			{
				Type:         "text",
				Text:         req.System,
				CacheControl: api.EphemeralCacheControl(),
			},
		}
		req.System = ""
	}

	if n := len(req.Tools); n > 0 {
		// Copy the slice so we don't mutate the shared loop.Tools backing array.
		copied := make([]api.Tool, n)
		copy(copied, req.Tools)
		copied[n-1].CacheControl = api.EphemeralCacheControl()
		req.Tools = copied
	}
}

// allTools returns built-in tools merged with any MCP tools.
func (loop *ConversationLoop) allTools() []api.Tool {
	if loop.MCPRegistry == nil {
		return loop.Tools
	}
	mcpAPITools := loop.MCPRegistry.AllAPITools()
	if len(mcpAPITools) == 0 {
		return loop.Tools
	}
	combined := make([]api.Tool, 0, len(loop.Tools)+len(mcpAPITools))
	combined = append(combined, loop.Tools...)
	combined = append(combined, mcpAPITools...)
	return combined
}

// baseLifeCtx returns a Context skeleton populated with the session-level
// fields (SessionID, WorkDir) common to all events.
func (loop *ConversationLoop) baseLifeCtx(event lifehooks.Event) lifehooks.Context {
	c := lifehooks.Context{Event: event, WorkDir: loop.workspaceRoot()}
	if loop.Session != nil {
		c.SessionID = loop.Session.ID
	}
	return c
}

// fireUserPromptSubmit dispatches a UserPromptSubmit event and applies any
// Modify decision to the prompt. Returns the (possibly rewritten) prompt and
// a boolean indicating whether the runtime should reject the prompt entirely
// (Block decision).
func (loop *ConversationLoop) fireUserPromptSubmit(ctx context.Context, prompt string) (string, bool, string) {
	if loop.LifecycleHooks == nil {
		return prompt, false, ""
	}
	hctx := loop.baseLifeCtx(lifehooks.UserPromptSubmit)
	hctx.UserPrompt = prompt
	dec, _ := loop.LifecycleHooks.Fire(ctx, hctx)
	switch dec.Action {
	case lifehooks.ActionBlock:
		return prompt, true, dec.Reason
	case lifehooks.ActionModify:
		if dec.Replacement != nil && dec.Replacement.UserPrompt != "" {
			return dec.Replacement.UserPrompt, false, ""
		}
	}
	return prompt, false, ""
}

// EmitStop fires the Stop lifecycle hook. Callers should invoke this once at
// the end of a session (e.g. when tearing down the conversation loop). It is
// a no-op when LifecycleHooks is nil.
func (loop *ConversationLoop) EmitStop(ctx context.Context) {
	if loop.LifecycleHooks == nil {
		return
	}
	_, _ = loop.LifecycleHooks.Fire(ctx, loop.baseLifeCtx(lifehooks.Stop))
}

// SendMessage sends a user message and runs the full agentic loop.
func (loop *ConversationLoop) SendMessage(ctx context.Context, userText string) error {
	// Lifecycle: UserPromptSubmit may rewrite or block the prompt.
	rewritten, blocked, reason := loop.fireUserPromptSubmit(ctx, userText)
	if blocked {
		if reason == "" {
			reason = "user prompt rejected by hook"
		}
		return fmt.Errorf("hook blocked user prompt: %s", reason)
	}
	userText = rewritten

	// Append user message
	loop.Session.Messages = append(loop.Session.Messages, api.Message{
		Role: "user",
		Content: []api.ContentBlock{
			{Type: "text", Text: userText},
		},
	})

	// Compact history if approaching the token budget (Phase 6).
	if ShouldCompact(loop.Compaction.LastInputTokens, loop.Session.Messages, loop.Config) {
		if loop.tryCompact(ctx) {
			loop.Compaction.CompactionCount++
		}
	}

	// Agentic loop: keep going until stop_reason is "end_turn"
	for {
		stopReason, err := loop.runOneTurn(ctx)
		if err != nil {
			return err
		}

		if stopReason != "tool_use" {
			break
		}
	}

	return nil
}

// runOneTurn sends the current session messages to the API and processes the response.
// Returns the stop_reason.
func (loop *ConversationLoop) runOneTurn(ctx context.Context) (string, error) {
	req := api.CreateMessageRequest{
		Model:     loop.Config.Model,
		MaxTokens: loop.Config.MaxTokens,
		System:    loop.systemPrompt(),
		Messages:  loop.Session.Messages,
		Tools:     loop.allTools(),
		Stream:    true,
	}
	loop.injectCacheControl(&req)

	ch, err := loop.Client.StreamResponse(ctx, req)
	if err != nil {
		return "", fmt.Errorf("stream response: %w", err)
	}

	// Accumulators for the current response
	type toolBlock struct {
		id          string
		name        string
		inputBuffer string
	}

	var (
		textBlocks   []api.ContentBlock
		toolBlocks   []toolBlock
		currentText  string
		currentTool  *toolBlock
		stopReason   string
		blockTypeMap = make(map[int]string) // index -> "text" or "tool_use"
	)

	for event := range ch {
		switch event.Type {
		case api.EventError:
			return "", fmt.Errorf("stream error: %s", event.ErrorMessage)

		case api.EventContentBlockStart:
			blockTypeMap[event.Index] = event.ContentBlock.Type
			if event.ContentBlock.Type == "tool_use" {
				tb := toolBlock{
					id:   event.ContentBlock.ID,
					name: event.ContentBlock.Name,
				}
				toolBlocks = append(toolBlocks, tb)
				currentTool = &toolBlocks[len(toolBlocks)-1]
			}

		case api.EventContentBlockDelta:
			switch event.Delta.Type {
			case "text_delta":
				currentText += event.Delta.Text
				fmt.Fprint(os.Stdout, event.Delta.Text)

			case "input_json_delta":
				if currentTool != nil {
					currentTool.inputBuffer += event.Delta.PartialJSON
				}
			}

		case api.EventContentBlockStop:
			bType, ok := blockTypeMap[event.Index]
			if ok && bType == "text" && currentText != "" {
				textBlocks = append(textBlocks, api.ContentBlock{
					Type: "text",
					Text: currentText,
				})
				currentText = ""
			}
			// Reset currentTool pointer (but keep toolBlocks slice)
			currentTool = nil

		case api.EventMessageDelta:
			stopReason = event.StopReason

		case api.EventMessageStop:
			// Stream complete
		}
	}

	// Ensure trailing newline after streaming text
	if len(textBlocks) > 0 || len(toolBlocks) > 0 {
		fmt.Fprintln(os.Stdout)
	}

	// Build the assistant message content
	var assistantContent []api.ContentBlock

	// Add text blocks first
	assistantContent = append(assistantContent, textBlocks...)

	// Add tool_use blocks
	for _, tb := range toolBlocks {
		var inputMap map[string]any
		if tb.inputBuffer != "" {
			if err := json.Unmarshal([]byte(tb.inputBuffer), &inputMap); err != nil {
				inputMap = map[string]any{"raw": tb.inputBuffer}
			}
		} else {
			inputMap = map[string]any{}
		}

		assistantContent = append(assistantContent, api.ContentBlock{
			Type:  "tool_use",
			ID:    tb.id,
			Name:  tb.name,
			Input: inputMap,
		})
	}

	// Append assistant message to session
	if len(assistantContent) > 0 {
		loop.Session.Messages = append(loop.Session.Messages, api.Message{
			Role:    "assistant",
			Content: assistantContent,
		})
	}

	// If stop_reason is tool_use, execute tools and append results
	if stopReason == "tool_use" {
		var toolResults []api.ContentBlock

		for _, tb := range toolBlocks {
			var inputMap map[string]any
			for _, cb := range assistantContent {
				if cb.Type == "tool_use" && cb.ID == tb.id {
					inputMap = cb.Input
					break
				}
			}
			if inputMap == nil {
				inputMap = map[string]any{}
			}

			fmt.Fprintf(os.Stdout, "\n[Tool: %s]\n", tb.name)
			result := loop.ExecuteTool(ctx, tb.name, inputMap)
			result.ToolUseID = tb.id
			toolResults = append(toolResults, result)
		}

		// Append tool results as a user message
		loop.Session.Messages = append(loop.Session.Messages, api.Message{
			Role:    "user",
			Content: toolResults,
		})
	}

	return stopReason, nil
}

// SendMessageStreaming sends a user message and runs the full agentic loop, emitting
// TurnEvents to the provided channel. The channel is NOT closed by this function;
// callers should close it after this returns.
func (loop *ConversationLoop) SendMessageStreaming(ctx context.Context, userText string, events chan<- TurnEvent) error {
	// Lifecycle: UserPromptSubmit may rewrite or block the prompt.
	rewritten, blocked, reason := loop.fireUserPromptSubmit(ctx, userText)
	if blocked {
		if reason == "" {
			reason = "user prompt rejected by hook"
		}
		err := fmt.Errorf("hook blocked user prompt: %s", reason)
		events <- TurnEvent{Type: TurnEventError, Err: err}
		return err
	}
	userText = rewritten

	loop.Session.Messages = append(loop.Session.Messages, api.Message{
		Role: "user",
		Content: []api.ContentBlock{
			{Type: "text", Text: userText},
		},
	})

	// Compact history if approaching the token budget (Phase 6).
	if ShouldCompact(loop.Compaction.LastInputTokens, loop.Session.Messages, loop.Config) {
		if loop.tryCompact(ctx) {
			loop.Compaction.CompactionCount++
		}
	}

	var totalInput, totalOutput, totalCacheWrite, totalCacheRead int

	for {
		stopReason, inTok, outTok, cwTok, crTok, err := loop.runOneTurnStreaming(ctx, events)
		if err != nil {
			events <- TurnEvent{Type: TurnEventError, Err: err}
			return err
		}
		totalInput += inTok
		totalOutput += outTok
		totalCacheWrite += cwTok
		totalCacheRead += crTok

		if stopReason != "tool_use" {
			break
		}
	}

	// Update compaction state with the latest token counts (Phase 6).
	loop.Compaction.LastInputTokens = totalInput
	loop.Compaction.TotalInputTokens += totalInput
	loop.Compaction.TotalOutputTokens += totalOutput

	// Update the usage tracker (Phase 13).
	if loop.Usage != nil {
		loop.Usage.Add(totalInput, totalOutput, totalCacheWrite, totalCacheRead)
	}

	events <- TurnEvent{
		Type:         TurnEventUsage,
		InputTokens:  totalInput,
		OutputTokens: totalOutput,
	}
	events <- TurnEvent{Type: TurnEventDone}
	return nil
}

// runOneTurnStreaming streams one API turn and sends TurnEvents.
// Returns stop_reason, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens, error.
func (loop *ConversationLoop) runOneTurnStreaming(ctx context.Context, events chan<- TurnEvent) (string, int, int, int, int, error) {
	req := api.CreateMessageRequest{
		Model:     loop.Config.Model,
		MaxTokens: loop.Config.MaxTokens,
		System:    loop.systemPrompt(),
		Messages:  loop.Session.Messages,
		Tools:     loop.allTools(),
		Stream:    true,
	}
	loop.injectCacheControl(&req)

	ch, err := loop.Client.StreamResponse(ctx, req)
	if err != nil {
		return "", 0, 0, 0, 0, fmt.Errorf("stream response: %w", err)
	}

	type toolBlock struct {
		id          string
		name        string
		inputBuffer string
	}

	var (
		textBlocks       []api.ContentBlock
		toolBlocks       []toolBlock
		currentText      string
		currentTool      *toolBlock
		stopReason       string
		blockTypeMap     = make(map[int]string)
		inputTokens      int
		outputTokens     int
		cacheWriteTokens int
		cacheReadTokens  int
	)

	for event := range ch {
		switch event.Type {
		case api.EventError:
			return "", 0, 0, 0, 0, fmt.Errorf("stream error: %s", event.ErrorMessage)

		case api.EventMessageStart:
			inputTokens = event.InputTokens
			cacheWriteTokens = event.CacheCreationInputTokens
			cacheReadTokens = event.CacheReadInputTokens

		case api.EventContentBlockStart:
			blockTypeMap[event.Index] = event.ContentBlock.Type
			if event.ContentBlock.Type == "tool_use" {
				tb := toolBlock{
					id:   event.ContentBlock.ID,
					name: event.ContentBlock.Name,
				}
				toolBlocks = append(toolBlocks, tb)
				currentTool = &toolBlocks[len(toolBlocks)-1]
			}

		case api.EventContentBlockDelta:
			switch event.Delta.Type {
			case "text_delta":
				currentText += event.Delta.Text
				select {
				case events <- TurnEvent{Type: TurnEventTextDelta, Text: event.Delta.Text}:
				case <-ctx.Done():
					return "", 0, 0, 0, 0, ctx.Err()
				}
			case "input_json_delta":
				if currentTool != nil {
					currentTool.inputBuffer += event.Delta.PartialJSON
				}
			}

		case api.EventContentBlockStop:
			if bType, ok := blockTypeMap[event.Index]; ok && bType == "text" && currentText != "" {
				textBlocks = append(textBlocks, api.ContentBlock{Type: "text", Text: currentText})
				currentText = ""
			}
			currentTool = nil

		case api.EventMessageDelta:
			stopReason = event.StopReason
			outputTokens = event.Usage.OutputTokens

		case api.EventMessageStop:
			// stream complete
		}
	}

	// Build assistant message content
	var assistantContent []api.ContentBlock
	assistantContent = append(assistantContent, textBlocks...)

	for _, tb := range toolBlocks {
		var inputMap map[string]any
		if tb.inputBuffer != "" {
			if err := json.Unmarshal([]byte(tb.inputBuffer), &inputMap); err != nil {
				inputMap = map[string]any{"raw": tb.inputBuffer}
			}
		} else {
			inputMap = map[string]any{}
		}
		assistantContent = append(assistantContent, api.ContentBlock{
			Type:  "tool_use",
			ID:    tb.id,
			Name:  tb.name,
			Input: inputMap,
		})
	}

	if len(assistantContent) > 0 {
		loop.Session.Messages = append(loop.Session.Messages, api.Message{
			Role:    "assistant",
			Content: assistantContent,
		})
	}

	// Execute tools if needed
	if stopReason == "tool_use" {
		var toolResults []api.ContentBlock

		for _, tb := range toolBlocks {
			var inputMap map[string]any
			for _, cb := range assistantContent {
				if cb.Type == "tool_use" && cb.ID == tb.id {
					inputMap = cb.Input
					break
				}
			}
			if inputMap == nil {
				inputMap = map[string]any{}
			}

			summary := summarizeToolInput(inputMap)

			// --- Permission check (Phase 5) ---
			if loop.PermManager != nil {
				decision := loop.PermManager.CheckCtx(ctx, tb.name, summary)

				// Plan mode: describe without executing.
				if loop.PermManager.Mode == permissions.ModePlan {
					planResult := api.ContentBlock{
						Type:      "tool_result",
						ToolUseID: tb.id,
						Content:   []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("[Plan: %s %s]", tb.name, summary)}},
					}
					toolResults = append(toolResults, planResult)
					continue
				}

				switch decision {
				case permissions.DecisionDeny:
					denied := api.ContentBlock{
						Type:      "tool_result",
						ToolUseID: tb.id,
						Content:   []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("Permission denied for tool: %s", tb.name)}},
						IsError:   true,
					}
					toolResults = append(toolResults, denied)
					continue

				case permissions.DecisionAsk:
					replyCh := make(chan PermDecision, 1)
					select {
					case events <- TurnEvent{
						Type:      TurnEventPermissionAsk,
						ToolName:  tb.name,
						ToolInput: summary,
						PermReply: replyCh,
					}:
					case <-ctx.Done():
						return "", 0, 0, 0, 0, ctx.Err()
					}

					var userDecision PermDecision
					select {
					case userDecision = <-replyCh:
					case <-ctx.Done():
						return "", 0, 0, 0, 0, ctx.Err()
					}

					switch userDecision {
					case PermDecisionDeny:
						denied := api.ContentBlock{
							Type:      "tool_result",
							ToolUseID: tb.id,
							Content:   []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("Permission denied for tool: %s", tb.name)}},
							IsError:   true,
						}
						toolResults = append(toolResults, denied)
						continue
					case PermDecisionAllowAlways:
						loop.PermManager.Remember(tb.name, summary, permissions.DecisionAllow, permissions.ScopeAlways)
					}
					// PermDecisionAllowOnce falls through to execution
				}
				// DecisionAllow falls through to execution
			}

			// ask_user: surface the question to the caller and wait for a reply.
			if tb.name == "ask_user" {
				question, _ := tools.AskUserInput(inputMap)
				if question == "" {
					question = "?"
				}
				replyCh := make(chan string, 1)
				select {
				case events <- TurnEvent{
					Type:         TurnEventAskUser,
					ToolName:     tb.name,
					ToolInput:    question,
					AskUserReply: replyCh,
				}:
				case <-ctx.Done():
					return "", 0, 0, 0, 0, ctx.Err()
				}
				var answer string
				select {
				case answer = <-replyCh:
				case <-ctx.Done():
					return "", 0, 0, 0, 0, ctx.Err()
				}
				toolResults = append(toolResults, api.ContentBlock{
					Type:      "tool_result",
					ToolUseID: tb.id,
					Content:   []api.ContentBlock{{Type: "text", Text: answer}},
				})
				continue
			}

			select {
			case events <- TurnEvent{Type: TurnEventToolStart, ToolName: tb.name, ToolInput: summary}:
			case <-ctx.Done():
				return "", 0, 0, 0, 0, ctx.Err()
			}

			result := loop.ExecuteTool(ctx, tb.name, inputMap)
			result.ToolUseID = tb.id
			toolResults = append(toolResults, result)

			resultText := ""
			if len(result.Content) > 0 {
				resultText = result.Content[0].Text
			}
			select {
			case events <- TurnEvent{Type: TurnEventToolDone, ToolName: tb.name, ToolResult: resultText}:
			case <-ctx.Done():
				return "", 0, 0, 0, 0, ctx.Err()
			}
		}

		loop.Session.Messages = append(loop.Session.Messages, api.Message{
			Role:    "user",
			Content: toolResults,
		})
	}

	return stopReason, inputTokens, outputTokens, cacheWriteTokens, cacheReadTokens, nil
}

// Deprecated: ExecuteToolQuiet is kept for backward compatibility. New code
// should use ExecuteTool which includes hooks, plugin dispatch, and telemetry.
func (loop *ConversationLoop) ExecuteToolQuiet(ctx context.Context, name string, input map[string]any) api.ContentBlock {
	if !CheckPermission(loop.Permissions, name) {
		return api.ContentBlock{
			Type:    "tool_result",
			Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("Permission denied for tool: %s", name)}},
			IsError: true,
		}
	}

	var result string
	var err error

	switch name {
	case "bash":
		result, err = tools.ExecuteBash(input, loop.currentPermMode(), loop.workspaceRoot())
	case "read_file":
		result, err = tools.ExecuteReadFile(input)
	case "write_file":
		result, err = tools.ExecuteWriteFile(input)
	case "glob":
		result, err = tools.ExecuteGlob(input)
	case "grep":
		result, err = tools.ExecuteGrep(input)
	case "file_edit":
		result, err = tools.ExecuteFileEdit(input)
	case "web_fetch":
		result, err = tools.ExecuteWebFetch(input)
	case "web_search":
		result, err = tools.ExecuteWebSearch(input)
	case "ask_user":
		q, ok := tools.AskUserInput(input)
		if !ok {
			err = fmt.Errorf("ask_user: 'question' is required")
		} else {
			return tools.AskUserFallback(q)
		}
	case "todo_write":
		result, err = tools.ExecuteTodoWrite(input)
	// --- Batch 2: simple stateless tools ---
	case "sleep":
		result, err = tools.ExecuteSleep(input)
	case "config":
		result, err = tools.ExecuteConfig(input, loop.configMap())
	case "repl":
		result, err = tools.ExecuteREPL(input)
	case "notebook_edit":
		result, err = tools.ExecuteNotebookEdit(input)
	case "structured_output":
		result, err = tools.ExecuteStructuredOutput(input)
	case "enter_plan_mode":
		result, err = tools.ExecuteEnterPlanMode(&loop.PlanModeActive, loop.planModeStateDir())
	case "exit_plan_mode":
		result, err = tools.ExecuteExitPlanMode(&loop.PlanModeActive, loop.planModeStateDir())
	case "send_user_message":
		result, err = tools.ExecuteSendUserMessage(input)
	// --- Batch 2: task tools ---
	case "task_create":
		result, err = tools.ExecuteTaskCreate(input, loop.TaskRegistry)
	case "task_get":
		result, err = tools.ExecuteTaskGet(input, loop.TaskRegistry)
	case "task_list":
		result, err = tools.ExecuteTaskList(input, loop.TaskRegistry)
	case "task_output":
		result, err = tools.ExecuteTaskOutput(input, loop.TaskRegistry)
	case "task_stop":
		result, err = tools.ExecuteTaskStop(input, loop.TaskRegistry)
	case "task_update":
		result, err = tools.ExecuteTaskUpdate(input, loop.TaskRegistry)
	case "run_task_packet":
		result, err = tools.ExecuteRunTaskPacket(input, loop.TaskRegistry)
	// --- Batch 2: team tools ---
	case "team_create":
		result, err = tools.ExecuteTeamCreate(input, loop.TeamRegistry)
	case "team_get":
		result, err = tools.ExecuteTeamGet(input, loop.TeamRegistry)
	case "team_list":
		result, err = tools.ExecuteTeamList(input, loop.TeamRegistry)
	case "team_delete":
		result, err = tools.ExecuteTeamDelete(input, loop.TeamRegistry)
	// --- Batch 2: cron tools ---
	case "cron_create":
		result, err = tools.ExecuteCronCreate(input, loop.CronRegistry)
	case "cron_get":
		result, err = tools.ExecuteCronGet(input, loop.CronRegistry)
	case "cron_list":
		result, err = tools.ExecuteCronList(input, loop.CronRegistry)
	case "cron_delete":
		result, err = tools.ExecuteCronDelete(input, loop.CronRegistry)
	// --- Batch 2: remote trigger ---
	case "remote_trigger":
		result, err = tools.ExecuteRemoteTrigger(ctx, input)
	// --- Batch 2: orchestration tools ---
	case "agent":
		result, err = tools.ExecuteAgent(input)
	case "skill":
		result, err = tools.ExecuteSkill(input, loop.workspaceRoot())
	case "tool_search":
		result, err = tools.ExecuteToolSearch(input, loop.allTools())
	// --- Batch 3: worker tools ---
	case "worker_create":
		result, err = tools.ExecuteWorkerCreate(input, loop.WorkerRegistry)
	case "worker_get":
		result, err = tools.ExecuteWorkerGet(input, loop.WorkerRegistry)
	case "worker_observe":
		result, err = tools.ExecuteWorkerObserve(input, loop.WorkerRegistry)
	case "worker_resolve_trust":
		result, err = tools.ExecuteWorkerResolveTrust(input, loop.WorkerRegistry)
	case "worker_await_ready":
		result, err = tools.ExecuteWorkerAwaitReady(input, loop.WorkerRegistry)
	case "worker_send_prompt":
		result, err = tools.ExecuteWorkerSendPrompt(input, loop.WorkerRegistry)
	case "worker_restart":
		result, err = tools.ExecuteWorkerRestart(input, loop.WorkerRegistry)
	case "worker_terminate":
		result, err = tools.ExecuteWorkerTerminate(input, loop.WorkerRegistry)
	case "worker_observe_completion":
		result, err = tools.ExecuteWorkerObserveCompletion(input, loop.WorkerRegistry)
	// --- Batch 3: LSP tool ---
	case "lsp":
		result, err = tools.ExecuteLSP(input, loop.LspRegistry)
	// --- Batch 3: MCP resource/auth tools ---
	case "list_mcp_resources":
		result, err = tools.ExecuteListMcpResources(input, loop.MCPRegistry)
	case "read_mcp_resource":
		result, err = tools.ExecuteReadMcpResource(input, loop.MCPRegistry)
	case "mcp_auth":
		result, err = tools.ExecuteMcpAuth(input, loop.MCPRegistry, loop.McpAuthState)
	default:
		// Fall back to MCP registry.
		if loop.MCPRegistry != nil {
			if client, _, ok := loop.MCPRegistry.FindTool(name); ok {
				mcpResult, mcpErr := client.CallTool(ctx, name, input)
				if mcpErr != nil {
					return api.ContentBlock{
						Type:    "tool_result",
						Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("Error: %v", mcpErr)}},
						IsError: true,
					}
				}
				text := mcpResultText(mcpResult)
				return api.ContentBlock{
					Type:    "tool_result",
					Content: []api.ContentBlock{{Type: "text", Text: text}},
					IsError: mcpResult.IsError,
				}
			}
		}
		err = fmt.Errorf("unknown tool: %s", name)
	}

	isError := err != nil
	text := result
	if err != nil {
		text = fmt.Sprintf("Error: %v", err)
	}

	return api.ContentBlock{
		Type:    "tool_result",
		Content: []api.ContentBlock{{Type: "text", Text: text}},
		IsError: isError,
	}
}

// summarizeToolInput returns a short human-readable summary of tool inputs.
func summarizeToolInput(input map[string]any) string {
	for _, key := range []string{"command", "path", "file_path", "pattern", "url", "query", "question"} {
		if v, ok := input[key].(string); ok {
			if len(v) > 60 {
				return v[:60] + "..."
			}
			return v
		}
	}
	return ""
}

// ClearSession resets the conversation history in the current session.
func (loop *ConversationLoop) ClearSession() {
	loop.Session.Messages = []api.Message{}
}

// ListSessions returns all session IDs saved in the configured session directory.
func (loop *ConversationLoop) ListSessions() ([]string, error) {
	return ListSessions(loop.Config.SessionDir)
}

// SaveCurrentSession persists the active session to disk, including usage data.
func (loop *ConversationLoop) SaveCurrentSession() error {
	if loop.Usage != nil {
		loop.Session.TotalInputTokens = loop.Usage.TotalInput
		loop.Session.TotalOutputTokens = loop.Usage.TotalOutput
		loop.Session.TotalTurns = loop.Usage.Turns
	}
	return SaveSession(loop.Config.SessionDir, loop.Session)
}

// LoadNamedSession replaces the active session with one loaded from disk by ID.
// Usage tracker state is restored from persisted session data.
func (loop *ConversationLoop) LoadNamedSession(id string) error {
	sess, err := LoadSession(loop.Config.SessionDir, id)
	if err != nil {
		return err
	}
	loop.Session = sess
	if loop.Usage != nil && sess.TotalTurns > 0 {
		loop.Usage.TotalInput = sess.TotalInputTokens
		loop.Usage.TotalOutput = sess.TotalOutputTokens
		loop.Usage.Turns = sess.TotalTurns
	}
	return nil
}

// ListSessionsWithMeta returns metadata for all saved sessions, sorted newest first.
func (loop *ConversationLoop) ListSessionsWithMeta() ([]SessionMeta, error) {
	return ListSessionsWithMeta(loop.Config.SessionDir)
}

// MessageCount returns the number of messages in the active session.
func (loop *ConversationLoop) MessageCount() int {
	if loop.Session == nil {
		return 0
	}
	return len(loop.Session.Messages)
}

// ExecuteTool dispatches to the appropriate tool implementation.
// When HookRunner is set, pre-tool and post-tool hooks wrap the execution.
// When LifecycleHooks is set, in-process PreToolUse/PostToolUse hooks fire
// around the dispatch. A PreToolUse Block decision short-circuits with a
// synthetic refusal tool_result (no tool execution).
//
// ctx is propagated to lifecycle hooks (Fire) and to the MCP fallback path so
// callers can cancel long-running tool execution from above.
func (loop *ConversationLoop) ExecuteTool(ctx context.Context, name string, input map[string]any) api.ContentBlock {
	if !CheckPermission(loop.Permissions, name) {
		return api.ContentBlock{
			Type:    "tool_result",
			Content: []api.ContentBlock{{Type: "text", Text: fmt.Sprintf("Permission denied for tool: %s", name)}},
			IsError: true,
		}
	}

	// --- Lifecycle PreToolUse (in-process) ---
	// Block short-circuits with a synthetic refusal so the model sees a
	// tool_result indicating the tool did not run.
	newInput, blocked, blockReason := loop.fireLifecyclePreToolUse(ctx, name, input)
	if blocked {
		// Notify post hook so observers see the rejection symmetrically.
		loop.fireLifecyclePostToolUse(ctx, name, input, blockReason, fmt.Errorf("blocked by hook"))
		return api.ContentBlock{
			Type:    "tool_result",
			Content: []api.ContentBlock{{Type: "text", Text: blockReason}},
			IsError: true,
		}
	}
	input = newInput

	// Serialize input for hook payload.
	inputJSON, _ := json.Marshal(input)
	inputStr := string(inputJSON)

	// --- PreToolUse hooks ---
	var preHookMessages []string
	// Extract permission override from pre-hooks (Phase 3: wired when
	// PermissionContext support is added in Phase 4).
	var preHookPermOverride *hooks.PermissionDecision
	var preHookPermReason string
	if loop.HookRunner != nil {
		hookResult := loop.HookRunner.RunPreToolUse(ctx, name, inputStr)
		if hookResult.IsDenied() || hookResult.IsFailed() || hookResult.IsCancelled() {
			msg := strings.Join(hookResult.Messages, "\n")
			if msg == "" {
				msg = fmt.Sprintf("Hook denied tool %s", name)
			}
			return api.ContentBlock{
				Type:    "tool_result",
				Content: []api.ContentBlock{{Type: "text", Text: msg}},
				IsError: true,
			}
		}
		// Store pre-hook messages for merging after tool execution.
		preHookMessages = hookResult.Messages
		preHookPermOverride = hookResult.PermissionOverride
		preHookPermReason = hookResult.PermissionReason
		// Apply input mutation from hooks.
		if hookResult.UpdatedInput != "" {
			var updatedMap map[string]any
			if err := json.Unmarshal([]byte(hookResult.UpdatedInput), &updatedMap); err == nil {
				input = updatedMap
				inputStr = hookResult.UpdatedInput
			}
		}
	}
	// Wire hook permission override: if a pre-hook set a permission decision,
	// apply it. Matches Rust semantics:
	//   - Deny: enforced immediately (short-circuit).
	//   - Allow: check ask-rules first; if an ask-rule matches, fall through
	//     to the normal permission/ask flow instead of auto-allowing.
	//   - Ask: force the normal permission/ask flow (prompt in interactive
	//     mode, deny in non-interactive/CI mode).
	if preHookPermOverride != nil {
		switch *preHookPermOverride {
		case hooks.PermissionAllow:
			// Rust: ask-rules take precedence over hook allow.
			// Only remember as allowed when no ask-rule matches.
			if loop.PermManager != nil {
				inputSummary := summarizeToolInput(input)
				if !loop.PermManager.MatchesAskRule(name, inputSummary) {
					loop.PermManager.Remember(name, inputSummary, permissions.DecisionAllow, permissions.ScopeAlways)
				}
			}
		case hooks.PermissionDeny:
			msg := "Hook denied tool execution"
			if preHookPermReason != "" {
				msg = preHookPermReason
			}
			return api.ContentBlock{
				Type:    "tool_result",
				Content: []api.ContentBlock{{Type: "text", Text: msg}},
				IsError: true,
			}
		case hooks.PermissionAsk:
			// Hook requests interactive confirmation. Falls through to tool
			// execution; interactive prompting is handled in the streaming
			// path (runOneTurnStreaming).
		default:
			fmt.Fprintf(os.Stderr, "[hooks] warning: unknown permission decision %q from pre-tool-use hook; ignoring\n", string(*preHookPermOverride))
		}
	}

	// Increment the atomic tool-call counter (Rust parity: tracks total tool
	// executions for the session; used by LoopAdapter.ToolCallCount()).
	loop.toolCallCount.Add(1)

	// --- Telemetry: record tool execution start ---
	if loop.Tracer != nil {
		loop.Tracer.Record("tool_execute_start", map[string]any{
			"tool_name": name,
		})
	}

	var result string
	var err error

	switch name {
	case "bash":
		result, err = tools.ExecuteBash(input, loop.currentPermMode(), loop.workspaceRoot())
	case "read_file":
		result, err = tools.ExecuteReadFile(input)
	case "write_file":
		result, err = tools.ExecuteWriteFile(input)
	case "glob":
		result, err = tools.ExecuteGlob(input)
	case "grep":
		result, err = tools.ExecuteGrep(input)
	case "file_edit":
		result, err = tools.ExecuteFileEdit(input)
	case "web_fetch":
		result, err = tools.ExecuteWebFetch(input)
	case "web_search":
		result, err = tools.ExecuteWebSearch(input)
	case "ask_user":
		var askText string
		var askErr error
		if loop.Asker != nil {
			askText, askErr = tools.ExecuteAskUser(ctx, loop.Asker, input)
		} else {
			q, ok := tools.AskUserInput(input)
			if !ok {
				askErr = fmt.Errorf("ask_user: 'question' is required")
			} else {
				askText = tools.AskUserFallback(q).Content[0].Text
				fmt.Fprintf(os.Stdout, "%s\n", askText)
			}
		}
		cb := api.ContentBlock{Type: "tool_result"}
		if askErr != nil {
			cb.Content = []api.ContentBlock{{Type: "text", Text: askErr.Error()}}
			cb.IsError = true
		} else {
			cb.Content = []api.ContentBlock{{Type: "text", Text: askText}}
		}
		askText = hooks.MergeHookFeedback(preHookMessages, cb.Content[0].Text, cb.IsError)
		postResult := loop.runPostToolHooks(ctx, name, inputStr, askText, cb.IsError)
		if postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled() {
			askText = hooks.MergeHookFeedback(postResult.Messages, askText, true)
			cb.IsError = true
		} else {
			askText = hooks.MergeHookFeedback(postResult.Messages, askText, false)
		}
		cb.Content[0].Text = askText
		if loop.Tracer != nil {
			loop.Tracer.Record("tool_execute_end", map[string]any{
				"tool_name": name,
				"is_error":  cb.IsError,
			})
		}
		return cb
	case "todo_write":
		result, err = tools.ExecuteTodoWrite(input)
	// --- Batch 2: simple stateless tools ---
	case "sleep":
		result, err = tools.ExecuteSleep(input)
	case "config":
		result, err = tools.ExecuteConfig(input, loop.configMap())
	case "repl":
		result, err = tools.ExecuteREPL(input)
	case "notebook_edit":
		result, err = tools.ExecuteNotebookEdit(input)
	case "structured_output":
		result, err = tools.ExecuteStructuredOutput(input)
	case "enter_plan_mode":
		result, err = tools.ExecuteEnterPlanMode(&loop.PlanModeActive, loop.planModeStateDir())
	case "exit_plan_mode":
		result, err = tools.ExecuteExitPlanMode(&loop.PlanModeActive, loop.planModeStateDir())
	case "send_user_message":
		result, err = tools.ExecuteSendUserMessage(input)
	// --- Batch 2: task tools ---
	case "task_create":
		result, err = tools.ExecuteTaskCreate(input, loop.TaskRegistry)
	case "task_get":
		result, err = tools.ExecuteTaskGet(input, loop.TaskRegistry)
	case "task_list":
		result, err = tools.ExecuteTaskList(input, loop.TaskRegistry)
	case "task_output":
		result, err = tools.ExecuteTaskOutput(input, loop.TaskRegistry)
	case "task_stop":
		result, err = tools.ExecuteTaskStop(input, loop.TaskRegistry)
	case "task_update":
		result, err = tools.ExecuteTaskUpdate(input, loop.TaskRegistry)
	case "run_task_packet":
		result, err = tools.ExecuteRunTaskPacket(input, loop.TaskRegistry)
	// --- Batch 2: team tools ---
	case "team_create":
		result, err = tools.ExecuteTeamCreate(input, loop.TeamRegistry)
	case "team_get":
		result, err = tools.ExecuteTeamGet(input, loop.TeamRegistry)
	case "team_list":
		result, err = tools.ExecuteTeamList(input, loop.TeamRegistry)
	case "team_delete":
		result, err = tools.ExecuteTeamDelete(input, loop.TeamRegistry)
	// --- Batch 2: cron tools ---
	case "cron_create":
		result, err = tools.ExecuteCronCreate(input, loop.CronRegistry)
	case "cron_get":
		result, err = tools.ExecuteCronGet(input, loop.CronRegistry)
	case "cron_list":
		result, err = tools.ExecuteCronList(input, loop.CronRegistry)
	case "cron_delete":
		result, err = tools.ExecuteCronDelete(input, loop.CronRegistry)
	// --- Batch 2: remote trigger ---
	case "remote_trigger":
		result, err = tools.ExecuteRemoteTrigger(ctx, input)
	// --- Batch 2: orchestration tools ---
	case "agent":
		result, err = tools.ExecuteAgent(input)
	case "skill":
		result, err = tools.ExecuteSkill(input, loop.workspaceRoot())
	case "tool_search":
		result, err = tools.ExecuteToolSearch(input, loop.allTools())
	// --- Batch 3: worker tools ---
	case "worker_create":
		result, err = tools.ExecuteWorkerCreate(input, loop.WorkerRegistry)
	case "worker_get":
		result, err = tools.ExecuteWorkerGet(input, loop.WorkerRegistry)
	case "worker_observe":
		result, err = tools.ExecuteWorkerObserve(input, loop.WorkerRegistry)
	case "worker_resolve_trust":
		result, err = tools.ExecuteWorkerResolveTrust(input, loop.WorkerRegistry)
	case "worker_await_ready":
		result, err = tools.ExecuteWorkerAwaitReady(input, loop.WorkerRegistry)
	case "worker_send_prompt":
		result, err = tools.ExecuteWorkerSendPrompt(input, loop.WorkerRegistry)
	case "worker_restart":
		result, err = tools.ExecuteWorkerRestart(input, loop.WorkerRegistry)
	case "worker_terminate":
		result, err = tools.ExecuteWorkerTerminate(input, loop.WorkerRegistry)
	case "worker_observe_completion":
		result, err = tools.ExecuteWorkerObserveCompletion(input, loop.WorkerRegistry)
	// --- Batch 3: LSP tool ---
	case "lsp":
		result, err = tools.ExecuteLSP(input, loop.LspRegistry)
	// --- Batch 3: MCP resource/auth tools ---
	case "list_mcp_resources":
		result, err = tools.ExecuteListMcpResources(input, loop.MCPRegistry)
	case "read_mcp_resource":
		result, err = tools.ExecuteReadMcpResource(input, loop.MCPRegistry)
	case "mcp_auth":
		result, err = tools.ExecuteMcpAuth(input, loop.MCPRegistry, loop.McpAuthState)
	default:
		// Try plugin tools first.
		if loop.PluginRegistry != nil {
			pluginTools, ptErr := loop.PluginRegistry.AggregatedTools()
			if ptErr == nil {
				for i := range pluginTools {
					pt := &pluginTools[i]
					if pt.Definition.Name == name {
						ptResult, ptExecErr := pt.Execute(json.RawMessage(inputJSON))
						ptIsError := ptExecErr != nil
						ptText := ptResult
						if ptExecErr != nil {
							ptText = fmt.Sprintf("Error: %v", ptExecErr)
							fmt.Fprintf(os.Stderr, "[Plugin tool %s error]: %v\n", name, ptExecErr)
						} else {
							fmt.Fprintf(os.Stdout, "%s\n", ptResult)
						}
						ptText = hooks.MergeHookFeedback(preHookMessages, ptText, ptIsError)
						postResult := loop.runPostToolHooks(ctx, name, inputStr, ptText, ptIsError)
						if postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled() {
							ptIsError = true
						}
						ptText = hooks.MergeHookFeedback(postResult.Messages, ptText,
							postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled())
						if loop.Tracer != nil {
							loop.Tracer.Record("tool_execute_end", map[string]any{
								"tool_name": name,
								"is_error":  ptIsError,
							})
						}
						return api.ContentBlock{
							Type:    "tool_result",
							Content: []api.ContentBlock{{Type: "text", Text: ptText}},
							IsError: ptIsError,
						}
					}
				}
			}
		}

		// Fall back to MCP registry.
		if loop.MCPRegistry != nil {
			if client, _, ok := loop.MCPRegistry.FindTool(name); ok {
				mcpResult, mcpErr := client.CallTool(ctx, name, input)
				if mcpErr != nil {
					fmt.Fprintf(os.Stderr, "[MCP tool %s error]: %v\n", name, mcpErr)
					mcpErrText := fmt.Sprintf("Error: %v", mcpErr)
					mcpErrText = hooks.MergeHookFeedback(preHookMessages, mcpErrText, true)
					postResult := loop.runPostToolHooks(ctx, name, inputStr, mcpErrText, true)
					mcpErrText = hooks.MergeHookFeedback(postResult.Messages, mcpErrText, true)
					return api.ContentBlock{
						Type:    "tool_result",
						Content: []api.ContentBlock{{Type: "text", Text: mcpErrText}},
						IsError: true,
					}
				}
				mcpText := mcpResultText(mcpResult)
				fmt.Fprintf(os.Stdout, "%s\n", mcpText)
				mcpIsError := mcpResult.IsError
				mcpText = hooks.MergeHookFeedback(preHookMessages, mcpText, false)
				postResult := loop.runPostToolHooks(ctx, name, inputStr, mcpText, mcpIsError)
				if postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled() {
					mcpIsError = true
				}
				mcpText = hooks.MergeHookFeedback(postResult.Messages, mcpText,
					postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled())
				return api.ContentBlock{
					Type:    "tool_result",
					Content: []api.ContentBlock{{Type: "text", Text: mcpText}},
					IsError: mcpIsError,
				}
			}
		}
		err = fmt.Errorf("unknown tool: %s", name)
	}

	isError := err != nil
	text := result
	if err != nil {
		text = fmt.Sprintf("Error: %v", err)
		fmt.Fprintf(os.Stderr, "[Tool %s error]: %v\n", name, err)
	} else {
		fmt.Fprintf(os.Stdout, "%s\n", result)
	}

	// --- Pre-hook message merging (Rust: merge_hook_feedback(pre_hook_result.messages(), output, false)) ---
	text = hooks.MergeHookFeedback(preHookMessages, text, false)

	// --- PostToolUse / PostToolUseFailure hooks ---
	postResult := loop.runPostToolHooks(ctx, name, inputStr, text, isError)
	if postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled() {
		isError = true
	}
	text = hooks.MergeHookFeedback(postResult.Messages, text,
		postResult.IsDenied() || postResult.IsFailed() || postResult.IsCancelled())

	// --- Telemetry: record tool execution end ---
	if loop.Tracer != nil {
		loop.Tracer.Record("tool_execute_end", map[string]any{
			"tool_name": name,
			"is_error":  isError,
		})
	}

	// --- Lifecycle PostToolUse / PostToolUseFailure (in-process) ---
	var postErr error
	if isError {
		postErr = err
		if postErr == nil {
			postErr = fmt.Errorf("tool %s failed", name)
		}
	}
	loop.fireLifecyclePostToolUse(ctx, name, input, text, postErr)

	return api.ContentBlock{
		Type: "tool_result",
		Content: []api.ContentBlock{
			{Type: "text", Text: text},
		},
		IsError: isError,
	}
}

// tryCompact wraps CompactSession with PreCompact / PostCompact lifecycle
// hooks. Returns true on successful compaction (so the caller can bump the
// CompactionCount). PreCompact Block decisions cause compaction to be
// skipped this turn.
func (loop *ConversationLoop) tryCompact(ctx context.Context) bool {
	msgCount := 0
	if loop.Session != nil {
		msgCount = len(loop.Session.Messages)
	}

	// PreCompact lifecycle hook.
	if loop.LifecycleHooks != nil {
		hctx := loop.baseLifeCtx(lifehooks.PreCompact)
		hctx.MessageCount = msgCount
		if dec, _ := loop.LifecycleHooks.Fire(ctx, hctx); dec.Action == lifehooks.ActionBlock {
			fmt.Fprintf(os.Stderr, "[compact] skipped: PreCompact hook blocked (%s)\n", dec.Reason)
			return false
		}
	}

	_, err := CompactSession(ctx, loop.Client, loop.Config, loop.Session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[compact] warning: %v\n", err)
		return false
	}

	if loop.LifecycleHooks != nil {
		hctx := loop.baseLifeCtx(lifehooks.PostCompact)
		if loop.Session != nil {
			hctx.MessageCount = len(loop.Session.Messages)
		}
		_, _ = loop.LifecycleHooks.Fire(ctx, hctx)
	}
	return true
}

// fireLifecyclePreToolUse runs the in-process PreToolUse hook. Returns
// (newInput, blocked, reason). If blocked is true, the runtime must skip
// tool execution and return a synthetic refusal tool_result.
//
// ctx is forwarded to LifecycleHooks.Fire so handlers can observe upstream
// cancellation (e.g. user interrupt during a long-running pre-hook).
func (loop *ConversationLoop) fireLifecyclePreToolUse(ctx context.Context, name string, input map[string]any) (map[string]any, bool, string) {
	if loop.LifecycleHooks == nil {
		return input, false, ""
	}
	hctx := loop.baseLifeCtx(lifehooks.PreToolUse)
	hctx.ToolName = name
	hctx.ToolInput = input
	dec, _ := loop.LifecycleHooks.Fire(ctx, hctx)
	switch dec.Action {
	case lifehooks.ActionBlock:
		reason := dec.Reason
		if reason == "" {
			reason = fmt.Sprintf("Tool %s blocked by hook", name)
		}
		return input, true, reason
	case lifehooks.ActionModify:
		if dec.Replacement != nil && dec.Replacement.ToolInput != nil {
			return dec.Replacement.ToolInput, false, ""
		}
	}
	return input, false, ""
}

// fireLifecyclePostToolUse runs the in-process PostToolUse or
// PostToolUseFailure hook. It is observational only — Block decisions are
// logged and ignored at this point because the tool has already executed.
//
// ctx is forwarded to LifecycleHooks.Fire so handlers can observe upstream
// cancellation.
func (loop *ConversationLoop) fireLifecyclePostToolUse(ctx context.Context, name string, input map[string]any, output string, toolErr error) {
	if loop.LifecycleHooks == nil {
		return
	}
	event := lifehooks.PostToolUse
	if toolErr != nil {
		event = lifehooks.PostToolUseFailure
	}
	hctx := loop.baseLifeCtx(event)
	hctx.ToolName = name
	hctx.ToolInput = input
	hctx.ToolResult = output
	hctx.ToolError = toolErr
	_, _ = loop.LifecycleHooks.Fire(ctx, hctx)
}

// runPostToolHooks runs PostToolUse or PostToolUseFailure hooks if a HookRunner is configured.
// Returns the HookRunResult so callers can check for denial/failure/cancellation
// and merge hook messages into tool output (matching Rust behavior). The
// caller's ctx is propagated so cancellation aborts in-flight hook scripts.
func (loop *ConversationLoop) runPostToolHooks(ctx context.Context, toolName, toolInput, toolOutput string, isError bool) hooks.HookRunResult {
	if loop.HookRunner == nil {
		return hooks.Allow(nil)
	}
	if isError {
		return loop.HookRunner.RunPostToolUseFailure(ctx, toolName, toolInput, toolOutput)
	}
	return loop.HookRunner.RunPostToolUse(ctx, toolName, toolInput, toolOutput, false)
}

// configMap returns a flat map of configuration values for the Config tool.
func (loop *ConversationLoop) configMap() map[string]any {
	if loop.Config == nil {
		return nil
	}
	return map[string]any{
		"model":           loop.Config.Model,
		"max_tokens":      loop.Config.MaxTokens,
		"permission_mode": loop.Config.PermissionMode,
		"session_dir":     loop.Config.SessionDir,
	}
}

// mcpResultText extracts the concatenated text from an MCP tool result.
func mcpResultText(r mcp.MCPToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// InitMCPFromConfig connects to all MCP servers defined in the config.
// Errors are printed but do not abort startup.
func (loop *ConversationLoop) InitMCPFromConfig(ctx context.Context) {
	if len(loop.Config.MCPServers) == 0 {
		return
	}
	if loop.MCPRegistry == nil {
		loop.MCPRegistry = mcp.NewRegistry()
	}
	for _, srv := range loop.Config.MCPServers {
		var transport mcp.Transport
		var err error

		switch strings.ToLower(srv.Transport) {
		case "stdio":
			var envPairs []string
			for k, v := range srv.Env {
				envPairs = append(envPairs, k+"="+v)
			}
			transport, err = mcp.NewStdioTransport(srv.Command, srv.Args, envPairs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[MCP] failed to start stdio server %q: %v\n", srv.Name, err)
				continue
			}
		case "sse", "http":
			auth := ""
			if tok, ok := srv.Env["AUTHORIZATION"]; ok {
				auth = tok
			}
			transport = mcp.NewSSETransport(srv.URL, auth)
		default:
			fmt.Fprintf(os.Stderr, "[MCP] unknown transport %q for server %q\n", srv.Transport, srv.Name)
			continue
		}

		if err := loop.MCPRegistry.AddServer(ctx, srv.Name, transport); err != nil {
			fmt.Fprintf(os.Stderr, "[MCP] failed to connect to server %q: %v\n", srv.Name, err)
		} else {
			toolCount := len(loop.MCPRegistry.ServerTools(srv.Name))
			fmt.Fprintf(os.Stdout, "[MCP] connected to %q (%d tools)\n", srv.Name, toolCount)
		}
	}
}

// MCPConnect connects to a named MCP server defined in config.
func (loop *ConversationLoop) MCPConnect(ctx context.Context, name string) error {
	if loop.MCPRegistry == nil {
		loop.MCPRegistry = mcp.NewRegistry()
	}
	for _, srv := range loop.Config.MCPServers {
		if srv.Name != name {
			continue
		}
		var transport mcp.Transport
		var err error
		switch strings.ToLower(srv.Transport) {
		case "stdio":
			var envPairs []string
			for k, v := range srv.Env {
				envPairs = append(envPairs, k+"="+v)
			}
			transport, err = mcp.NewStdioTransport(srv.Command, srv.Args, envPairs)
			if err != nil {
				return err
			}
		case "sse", "http":
			auth := ""
			if tok, ok := srv.Env["AUTHORIZATION"]; ok {
				auth = tok
			}
			transport = mcp.NewSSETransport(srv.URL, auth)
		default:
			return fmt.Errorf("unknown transport %q", srv.Transport)
		}
		return loop.MCPRegistry.AddServer(ctx, name, transport)
	}
	return fmt.Errorf("MCP server %q not found in config", name)
}

// MCPDisconnect disconnects from a named MCP server.
func (loop *ConversationLoop) MCPDisconnect(name string) error {
	if loop.MCPRegistry == nil {
		return fmt.Errorf("no MCP servers connected")
	}
	return loop.MCPRegistry.Disconnect(name)
}

// MCPList returns a human-readable summary of connected MCP servers and their tools.
func (loop *ConversationLoop) MCPList() string {
	if loop.MCPRegistry == nil {
		return "No MCP servers connected.\n"
	}
	names := loop.MCPRegistry.ServerNames()
	if len(names) == 0 {
		return "No MCP servers connected.\n"
	}
	var sb strings.Builder
	for _, name := range names {
		tools := loop.MCPRegistry.ServerTools(name)
		fmt.Fprintf(&sb, "Server: %s (%d tools)\n", name, len(tools))
		for _, t := range tools {
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:60] + "..."
			}
			fmt.Fprintf(&sb, "  - %s: %s\n", t.Name, desc)
		}
	}
	return sb.String()
}

// InitHooksFromConfig creates a HookRunner from the config's hook commands
// merged with any plugin-provided hooks. This wires the hooks system into
// the execution loop so that PreToolUse/PostToolUse/PostToolUseFailure hooks
// run around every tool dispatch.
func (loop *ConversationLoop) InitHooksFromConfig(configHooks hooks.HookConfig) {
	merged := configHooks

	// Merge plugin hooks if a PluginRegistry is available.
	if loop.PluginRegistry != nil {
		pluginHooks, err := loop.PluginRegistry.AggregatedHooks()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[hooks] warning: failed to aggregate plugin hooks: %v\n", err)
		} else {
			merged = hooks.MergeConfigs(merged, hooks.HookConfig{
				PreToolUse:         pluginHooks.PreToolUse,
				PostToolUse:        pluginHooks.PostToolUse,
				PostToolUseFailure: pluginHooks.PostToolUseFailure,
			})
		}
	}

	// Only create a runner if there are hooks to run.
	if len(merged.PreToolUse) > 0 || len(merged.PostToolUse) > 0 || len(merged.PostToolUseFailure) > 0 {
		loop.HookRunner = hooks.NewHookRunner(merged)
	}
}

// InitTelemetry sets up the telemetry sink and session tracer.
// If path is empty, telemetry is disabled (NopTelemetrySink).
func (loop *ConversationLoop) InitTelemetry(path string) {
	if path == "" {
		loop.TelemetrySink = apikit.NopTelemetrySink{}
		return
	}

	sink, err := apikit.NewJsonlTelemetrySink(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[telemetry] warning: failed to open sink at %s: %v\n", path, err)
		loop.TelemetrySink = apikit.NopTelemetrySink{}
		return
	}

	loop.TelemetrySink = sink
	if loop.Session != nil {
		loop.Tracer = apikit.NewSessionTracer(loop.Session.ID, sink)
	}

	// Wire the tracer to the API client if it supports it.
	if loop.Tracer != nil {
		if c, ok := loop.Client.(*api.Client); ok {
			c.Tracer = loop.Tracer
		}
	}
}

// InitPlugins discovers and initializes plugins, wiring their tools and hooks.
func (loop *ConversationLoop) InitPlugins(registry *plugin.PluginRegistry) {
	if registry == nil {
		return
	}
	loop.PluginRegistry = registry

	// Register plugin tools as API tools.
	pluginTools, err := registry.AggregatedTools()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[plugins] warning: failed to aggregate tools: %v\n", err)
		return
	}
	for _, pt := range pluginTools {
		var schema api.InputSchema
		if pt.Definition.InputSchema != nil {
			// Best-effort parse of the raw JSON schema into InputSchema.
			_ = json.Unmarshal(pt.Definition.InputSchema, &schema)
		}
		if schema.Type == "" {
			schema.Type = "object"
		}
		loop.Tools = append(loop.Tools, api.Tool{
			Name:        pt.Definition.Name,
			Description: pt.Definition.Description,
			InputSchema: schema,
		})
	}
}

// HandleSlashCommand dispatches a slash command through the LoopAdapter and
// CommandRegistry. Returns (handled, error). If the command is not a slash
// command (doesn't start with /), returns (false, nil).
func (loop *ConversationLoop) HandleSlashCommand(input string) (bool, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return false, nil
	}
	if loop.CommandRegistry == nil {
		return false, nil
	}

	// Create a LoopAdapter to satisfy all command interface contracts.
	adapter := NewLoopAdapter(loop)
	adapter.SetBuildInfo(loop.BuildVersion, loop.BuildCommit)
	adapter.SetPluginManager(loop.MarketplaceMgr)

	// Type-assert to *commands.Registry interface.
	type commandExecutor interface {
		Execute(input string, loop interface{}) (bool, error)
	}
	reg, ok := loop.CommandRegistry.(commandExecutor)
	if !ok {
		return false, nil
	}

	return reg.Execute(input, adapter)
}
