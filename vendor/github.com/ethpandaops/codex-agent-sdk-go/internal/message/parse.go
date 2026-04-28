package message

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
)

func newAuditEnvelope(data map[string]any) (*AuditEnvelope, error) {
	payload, ok := extractRawJSON(data)
	if !ok {
		var err error

		payload, err = json.Marshal(stripRawJSON(data))
		if err != nil {
			return nil, fmt.Errorf("marshal audit payload: %w", err)
		}
	}

	eventType, _ := data["type"].(string)
	subtype, _ := data["subtype"].(string)

	return &AuditEnvelope{
		EventType: eventType,
		Subtype:   subtype,
		Payload:   payload,
	}, nil
}

// Parse converts a raw JSON map into a typed Message.
//
// This function handles both Claude-style messages (with "type": "user"|"assistant"|etc.)
// and Codex-style events (with "type": "thread.started"|"item.completed"|etc.).
func Parse(log *slog.Logger, payload any) (Message, error) {
	log = log.With("component", "message_parser")

	var data map[string]any

	switch raw := payload.(type) {
	case map[string]any:
		data = raw
	case []byte:
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, &errors.MessageParseError{
				Message: err.Error(),
				Err:     err,
			}
		}

		data = AnnotateRawJSON(data, raw)
	case json.RawMessage:
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, &errors.MessageParseError{
				Message: err.Error(),
				Err:     err,
			}
		}

		data = AnnotateRawJSON(data, raw)
	default:
		return nil, &errors.MessageParseError{
			Message: fmt.Sprintf(
				"unsupported payload type %T", payload,
			),
			Err: fmt.Errorf("unsupported payload type %T", payload),
		}
	}

	msgType, ok := data["type"].(string)
	if !ok {
		return nil, &errors.MessageParseError{
			Message: "missing or invalid 'type' field",
			Err:     fmt.Errorf("missing or invalid 'type' field"),
			Data:    data,
		}
	}

	log.Debug("parsing message", slog.String("message_type", msgType))

	// Try Claude-style message types first
	var (
		msg Message
		err error
	)

	switch msgType {
	case "user":
		msg, err = parseUserMessage(data)
	case "assistant":
		msg, err = parseAssistantMessage(data)
	case "system":
		msg, err = parseSystemMessage(data)
	case "result":
		msg, err = parseResultMessage(data)
	case "stream_event":
		msg, err = parseStreamEvent(data)
	default:
		msg, err = parseCodexEvent(log, data, EventType(msgType))
	}

	if err != nil {
		return nil, err
	}

	audit, err := newAuditEnvelope(data)
	if err != nil {
		return nil, err
	}

	attachAudit(msg, audit)

	return msg, nil
}

func attachAudit(msg Message, audit *AuditEnvelope) {
	switch typed := msg.(type) {
	case *UserMessage:
		typed.Audit = audit
	case *AssistantMessage:
		typed.Audit = audit
	case *SystemMessage:
		typed.Audit = audit
	case *TaskStartedMessage:
		typed.Audit = audit
	case *TaskCompleteMessage:
		typed.Audit = audit
	case *ThreadRolledBackMessage:
		typed.Audit = audit
	case *ResultMessage:
		typed.Audit = audit
	case *StreamEvent:
		typed.Audit = audit
	}
}

// parseCodexEvent converts a Codex event into a claude-sdk-compatible Message.
func parseCodexEvent(
	log *slog.Logger,
	data map[string]any,
	eventType EventType,
) (Message, error) {
	switch eventType {
	case EventItemCompleted, EventItemStarted, EventItemUpdated:
		return parseCodexItemEvent(log, data)

	case EventTurnCompleted:
		return parseCodexTurnCompleted(data)

	case EventTurnFailed:
		return parseCodexTurnFailed(data)

	case EventThreadStarted, EventTurnStarted:
		// System-level events → SystemMessage
		return &SystemMessage{
			Type:    "system",
			Subtype: string(eventType),
			Data:    data,
		}, nil

	case EventError:
		msg, _ := data["message"].(string)
		errType := AssistantMessageErrorUnknown

		return &AssistantMessage{
			Type: "assistant",
			Content: []ContentBlock{
				&TextBlock{Type: BlockTypeText, Text: "Error: " + msg},
			},
			Error: &errType,
		}, nil

	default:
		return nil, errors.ErrUnknownMessageType
	}
}

// parseCodexItemEvent converts a Codex item event into an AssistantMessage.
func parseCodexItemEvent(log *slog.Logger, data map[string]any) (Message, error) {
	event, err := ParseCodexEvent(data)
	if err != nil {
		return nil, &errors.MessageParseError{
			Message: "failed to parse codex event",
			Err:     err,
			Data:    data,
		}
	}

	if event.Item == nil {
		log.Debug("codex event has no item", slog.String("event_type", string(event.Type)))

		return &SystemMessage{
			Type:    "system",
			Subtype: string(event.Type),
			Data:    data,
		}, nil
	}

	// Defense-in-depth: suppress non-completed agent_message events that
	// reach the parser (e.g. item.updated deltas from custom transports or
	// exec backend). Only item.completed agent_message events produce an
	// AssistantMessage; others are emitted as system lifecycle messages.
	if event.Item.Type == ItemTypeAgentMessage && event.Type != EventItemCompleted {
		return &SystemMessage{
			Type:    "system",
			Subtype: string(event.Type) + ".agent_message_delta",
			Data:    data,
		}, nil
	}

	// App-server emits empty reasoning items with no summary/content before
	// final answers. These are transport lifecycle noise, not user-visible
	// assistant messages.
	if event.Item.Type == ItemTypeReasoning && strings.TrimSpace(event.Item.Text) == "" {
		return &SystemMessage{
			Type:    "system",
			Subtype: string(event.Type) + ".reasoning_delta",
			Data:    data,
		}, nil
	}

	return convertCodexItem(event.Item, event.Type), nil
}

// convertCodexItem converts a single Codex item to an AssistantMessage.
func convertCodexItem(item *CodexItem, eventType EventType) *AssistantMessage {
	msg := &AssistantMessage{
		Type: "assistant",
	}

	switch item.Type {
	case ItemTypeAgentMessage:
		msg.Content = []ContentBlock{
			&TextBlock{Type: BlockTypeText, Text: item.Text},
		}

	case ItemTypeReasoning:
		msg.Content = []ContentBlock{
			&ThinkingBlock{Type: BlockTypeThinking, Thinking: item.Text},
		}

	case ItemTypeCommandExec:
		msg.Content = convertCommandExec(item, eventType)

	case ItemTypeFileChange:
		msg.Content = convertFileChange(item)

	case ItemTypeMCPToolCall:
		toolName := item.Tool
		if item.Server != "" {
			toolName = item.Server + ":" + item.Tool
		}

		msg.Content = convertCallableToolItem(item, eventType, toolName)

	case ItemTypeDynamicToolCall:
		toolName := item.Name
		if toolName == "" {
			toolName = item.Tool
		}

		msg.Content = convertCallableToolItem(
			item, eventType, normalizePublicMCPToolName(toolName),
		)

	case ItemTypeWebSearch:
		msg.Content = []ContentBlock{
			&ToolUseBlock{
				Type: BlockTypeToolUse,
				ID:   item.ID,
				Name: "WebSearch",
				Input: map[string]any{
					"query": item.Query,
				},
			},
		}

	case ItemTypeTodoList:
		msg.Content = convertTodoList(item)

	case ItemTypeToolSearch:
		toolName := item.Name
		if toolName == "" {
			toolName = "ToolSearch"
		}

		msg.Content = convertCallableToolItem(item, eventType, toolName)

	case ItemTypeImageGeneration:
		toolName := item.Name
		if toolName == "" {
			toolName = "ImageGeneration"
		}

		msg.Content = convertCallableToolItem(item, eventType, toolName)

	case ItemTypeCustomToolCall:
		toolName := item.Name
		if toolName == "" {
			toolName = item.Tool
		}

		if toolName == "" {
			toolName = "CustomTool"
		}

		msg.Content = convertCallableToolItem(item, eventType, toolName)

	case ItemTypeError:
		msg.Content = []ContentBlock{
			&TextBlock{Type: BlockTypeText, Text: "Error: " + item.Message},
		}
		errType := AssistantMessageErrorUnknown
		msg.Error = &errType

	default:
		// Unknown item type — preserve any available text or fall back to the
		// raw item payload so new Codex item variants don't collapse to empty
		// assistant messages.
		text := item.Text
		if strings.TrimSpace(text) == "" {
			text = stringifyRawItem(item.Raw)
		}

		msg.Content = []ContentBlock{
			&TextBlock{Type: BlockTypeText, Text: text},
		}
	}

	return msg
}

// convertCommandExec builds content blocks for a command_execution item.
func convertCommandExec(item *CodexItem, eventType EventType) []ContentBlock {
	toolUse := &ToolUseBlock{
		Type: BlockTypeToolUse,
		ID:   item.ID,
		Name: "Bash",
		Input: map[string]any{
			"command": item.Command,
		},
	}

	blocks := []ContentBlock{toolUse}

	if eventType == EventItemCompleted {
		result := &ToolResultBlock{
			Type:      BlockTypeToolResult,
			ToolUseID: item.ID,
			Content: []ContentBlock{
				&TextBlock{Type: BlockTypeText, Text: item.AggregatedOutput},
			},
		}

		if item.ExitCode != nil && *item.ExitCode != 0 {
			result.IsError = true
		}

		blocks = append(blocks, result)
	}

	return blocks
}

// convertFileChange builds content blocks for a file_change item.
func convertFileChange(item *CodexItem) []ContentBlock {
	toolName := "Edit"

	for _, change := range item.Changes {
		if change.Kind == "create" || change.Kind == "add" {
			toolName = "Write"

			break
		}
	}

	input := map[string]any{}
	if len(item.Changes) > 0 {
		input["file_path"] = item.Changes[0].Path
	}

	return []ContentBlock{
		&ToolUseBlock{
			Type:  BlockTypeToolUse,
			ID:    item.ID,
			Name:  toolName,
			Input: input,
		},
	}
}

// convertTodoList builds a text block from a todo_list item.
func convertTodoList(item *CodexItem) []ContentBlock {
	lines := make([]string, 0, len(item.Items))

	for _, todoItem := range item.Items {
		marker := "[ ]"
		if todoItem.Completed {
			marker = "[x]"
		}

		lines = append(lines, fmt.Sprintf("- %s %s", marker, todoItem.Text))
	}

	return []ContentBlock{
		&TextBlock{Type: BlockTypeText, Text: strings.Join(lines, "\n")},
	}
}

// convertCallableToolItem builds content blocks for tool call items that may
// include a result on completion (dynamic_tool_call, custom_tool_call).
func convertCallableToolItem(item *CodexItem, eventType EventType, toolName string) []ContentBlock {
	toolUse := &ToolUseBlock{
		Type:  BlockTypeToolUse,
		ID:    item.ID,
		Name:  toolName,
		Input: item.Arguments,
	}

	blocks := []ContentBlock{toolUse}

	if eventType == EventItemCompleted {
		result := &ToolResultBlock{
			Type:      BlockTypeToolResult,
			ToolUseID: item.ID,
			Content:   contentItemsToBlocks(item.ContentItems),
		}

		if item.Success != nil && !*item.Success {
			result.IsError = true
		}

		blocks = append(blocks, result)
	}

	return blocks
}

func contentItemsToBlocks(items []ContentItem) []ContentBlock {
	if len(items) == 0 {
		return nil
	}

	blocks := make([]ContentBlock, 0, len(items))

	for _, item := range items {
		switch item.Type {
		case "inputText", "text":
			blocks = append(blocks, &TextBlock{Type: BlockTypeText, Text: item.Text})
		case "inputImage":
			url := item.Text
			if url == "" {
				url, _ = item.Raw["imageUrl"].(string)
			}

			if url == "" {
				url, _ = item.Raw["image_url"].(string)
			}

			blocks = append(blocks, &InputImageBlock{Type: BlockTypeImage, URL: url})
		default:
			if rawText := stringifyContentItem(item); rawText != "" {
				blocks = append(blocks, &TextBlock{Type: BlockTypeText, Text: rawText})
			}
		}
	}

	return blocks
}

func stringifyContentItem(item ContentItem) string {
	if len(item.Raw) == 0 {
		return ""
	}

	data, err := json.Marshal(item.Raw)
	if err != nil {
		return ""
	}

	return string(data)
}

func stringifyRawItem(raw map[string]any) string {
	if len(raw) == 0 {
		return ""
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return ""
	}

	return string(data)
}

func normalizePublicMCPToolName(name string) string {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return name
	}

	rest := strings.TrimPrefix(name, prefix)

	idx := strings.Index(rest, "__")
	if idx <= 0 {
		return name
	}

	return rest[:idx] + ":" + rest[idx+2:]
}

// parseCodexTurnCompleted converts a turn.completed event to a ResultMessage.
func parseCodexTurnCompleted(data map[string]any) (*ResultMessage, error) {
	result := &ResultMessage{
		Type:    "result",
		Subtype: "success",
	}

	if v, ok := data["is_error"].(bool); ok {
		result.IsError = v
	}

	if sid, ok := data["session_id"].(string); ok {
		result.SessionID = sid
	}

	if txt, ok := data["result"].(string); ok {
		result.Result = &txt
	}

	if structured, ok := data["structured_output"]; ok {
		result.StructuredOutput = structured
	}

	// Parse usage if present
	if usageData, ok := data["usage"].(map[string]any); ok {
		usage := &Usage{}

		if v, ok := usageData["input_tokens"].(float64); ok {
			usage.InputTokens = int(v)
		}

		if v, ok := usageData["output_tokens"].(float64); ok {
			usage.OutputTokens = int(v)
		}

		if v, ok := usageData["cached_input_tokens"].(float64); ok {
			usage.CachedInputTokens = int(v)
		}

		if v, ok := usageData["reasoning_output_tokens"].(float64); ok {
			usage.ReasoningOutputTokens = int(v)
		}

		result.Usage = usage
	}

	if sr, ok := data["stop_reason"].(string); ok {
		result.StopReason = &sr
	}

	if dur, ok := data["duration_ms"].(float64); ok {
		result.DurationMs = int(dur)
	}

	if nt, ok := data["num_turns"].(float64); ok {
		result.NumTurns = int(nt)
	}

	if tc, ok := data["total_cost_usd"].(float64); ok {
		result.TotalCostUSD = &tc
	}

	return result, nil
}

// parseCodexTurnFailed converts a turn.failed event to a ResultMessage.
func parseCodexTurnFailed(data map[string]any) (*ResultMessage, error) {
	result := &ResultMessage{
		Type:    "result",
		Subtype: "error",
		IsError: true,
	}

	if errorData, ok := data["error"].(map[string]any); ok {
		if msg, ok := errorData["message"].(string); ok {
			result.Result = &msg
		}
	}

	return result, nil
}

// parseUserMessage parses a UserMessage from raw JSON.
func parseUserMessage(data map[string]any) (*UserMessage, error) {
	msg := &UserMessage{Type: "user"}

	messageData, ok := data["message"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("user message: missing or invalid 'message' field")
	}

	contentData, ok := messageData["content"]
	if !ok {
		return nil, fmt.Errorf("user message: missing content field")
	}

	contentJSON, err := json.Marshal(contentData)
	if err != nil {
		return nil, fmt.Errorf("user message: marshal content: %w", err)
	}

	var content UserMessageContent
	if err := json.Unmarshal(contentJSON, &content); err != nil {
		return nil, fmt.Errorf("user message: %w", err)
	}

	msg.Content = content

	if uuid, ok := data["uuid"].(string); ok {
		msg.UUID = &uuid
	}

	if parentToolUseID, ok := data["parent_tool_use_id"].(string); ok {
		msg.ParentToolUseID = &parentToolUseID
	}

	return msg, nil
}

// parseAssistantMessage parses an AssistantMessage from raw JSON.
func parseAssistantMessage(data map[string]any) (*AssistantMessage, error) {
	msg := &AssistantMessage{Type: "assistant"}

	messageData, ok := data["message"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing or invalid 'message' field")
	}

	if contentData, ok := messageData["content"].([]any); ok {
		content, err := parseContentBlocks(contentData)
		if err != nil {
			return nil, fmt.Errorf("parse assistant content: %w", err)
		}

		msg.Content = content
	}

	if model, ok := messageData["model"].(string); ok {
		msg.Model = model
	}

	if parentToolUseID, ok := data["parent_tool_use_id"].(string); ok {
		msg.ParentToolUseID = &parentToolUseID
	}

	if errorVal, ok := data["error"].(string); ok {
		errType := AssistantMessageError(errorVal)
		msg.Error = &errType
	}

	return msg, nil
}

func systemMessageData(data map[string]any) map[string]any {
	if msgData, ok := data["data"].(map[string]any); ok {
		return msgData
	}

	msgData := make(map[string]any, len(data))
	for k, v := range data {
		if k != "type" && k != "subtype" {
			msgData[k] = v
		}
	}

	return msgData
}

func requiredSystemMessageData(data map[string]any) (map[string]any, error) {
	msgData, ok := data["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("system message: missing or invalid %q field", "data")
	}

	return msgData, nil
}

func requiredStringField(data map[string]any, key string) (string, error) {
	value, ok := data[key].(string)
	if !ok {
		return "", fmt.Errorf("system message: missing or invalid %q field", key)
	}

	return value, nil
}

func parseTaskStartedSystemMessage(data map[string]any, system SystemMessage) (Message, error) {
	turnID, err := requiredStringField(data, "turn_id")
	if err != nil {
		return nil, err
	}

	msg := &TaskStartedMessage{
		SystemMessage: system,
		TurnID:        turnID,
	}

	if mode, ok := data["collaboration_mode_kind"].(string); ok {
		msg.CollaborationModeKind = mode
	}

	switch v := data["model_context_window"].(type) {
	case float64:
		modelContextWindow := int64(v)
		msg.ModelContextWindow = &modelContextWindow
	case int64:
		modelContextWindow := v
		msg.ModelContextWindow = &modelContextWindow
	}

	return msg, nil
}

func parseTaskCompleteSystemMessage(data map[string]any, system SystemMessage) (Message, error) {
	turnID, err := requiredStringField(data, "turn_id")
	if err != nil {
		return nil, err
	}

	msg := &TaskCompleteMessage{
		SystemMessage: system,
		TurnID:        turnID,
	}

	if lastAgentMessage, ok := data["last_agent_message"].(string); ok {
		msg.LastAgentMessage = &lastAgentMessage
	}

	return msg, nil
}

func parseThreadRolledBackSystemMessage(data map[string]any, system SystemMessage) (Message, error) {
	var numTurns int

	switch v := data["num_turns"].(type) {
	case float64:
		numTurns = int(v)
	case int:
		numTurns = v
	default:
		return nil, fmt.Errorf("system message: missing or invalid %q field", "num_turns")
	}

	return &ThreadRolledBackMessage{
		SystemMessage: system,
		NumTurns:      numTurns,
	}, nil
}

// parseSystemMessage parses a SystemMessage from raw JSON.
func parseSystemMessage(data map[string]any) (Message, error) {
	msg := &SystemMessage{Type: "system"}

	subtype, ok := data["subtype"].(string)
	if !ok {
		return nil, fmt.Errorf("system message: missing or invalid 'subtype' field")
	}

	msg.Subtype = subtype

	switch subtype {
	case "task.started":
		msgData, err := requiredSystemMessageData(data)
		if err != nil {
			return nil, err
		}

		msg.Data = msgData

		typed, err := parseTaskStartedSystemMessage(msg.Data, *msg)
		if err != nil {
			return nil, err
		}

		return typed, nil
	case "task.complete":
		msgData, err := requiredSystemMessageData(data)
		if err != nil {
			return nil, err
		}

		msg.Data = msgData

		typed, err := parseTaskCompleteSystemMessage(msg.Data, *msg)
		if err != nil {
			return nil, err
		}

		return typed, nil
	case "thread.rolled_back":
		msgData, err := requiredSystemMessageData(data)
		if err != nil {
			return nil, err
		}

		msg.Data = msgData

		typed, err := parseThreadRolledBackSystemMessage(msg.Data, *msg)
		if err != nil {
			return nil, err
		}

		return typed, nil
	default:
		msg.Data = systemMessageData(data)

		return msg, nil
	}
}

// parseStreamEvent parses a StreamEvent from raw JSON.
func parseStreamEvent(data map[string]any) (*StreamEvent, error) {
	event := &StreamEvent{}

	uuid, ok := data["uuid"].(string)
	if !ok {
		return nil, fmt.Errorf("stream_event: missing or invalid 'uuid' field")
	}

	event.UUID = uuid

	sessionID, ok := data["session_id"].(string)
	if !ok {
		return nil, fmt.Errorf("stream_event: missing or invalid 'session_id' field")
	}

	event.SessionID = sessionID

	eventData, ok := data["event"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stream_event: missing or invalid 'event' field")
	}

	event.Event = eventData

	if parentToolUseID, ok := data["parent_tool_use_id"].(string); ok {
		event.ParentToolUseID = &parentToolUseID
	}

	return event, nil
}

// parseResultMessage parses a ResultMessage from raw JSON.
func parseResultMessage(data map[string]any) (*ResultMessage, error) {
	if _, ok := data["subtype"].(string); !ok {
		return nil, fmt.Errorf("result message: missing or invalid 'subtype' field")
	}

	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}

	var msg ResultMessage
	if err := json.Unmarshal(jsonBytes, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal result: %w", err)
	}

	return &msg, nil
}

// parseContentBlocks parses an array of content blocks.
func parseContentBlocks(data []any) ([]ContentBlock, error) {
	blocks := make([]ContentBlock, 0, len(data))

	for i, item := range data {
		blockData, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content block %d: not an object", i)
		}

		block, err := parseContentBlock(blockData)
		if err != nil {
			return nil, fmt.Errorf("content block %d: %w", i, err)
		}

		blocks = append(blocks, block)
	}

	return blocks, nil
}

// parseContentBlock parses a single content block from a map.
func parseContentBlock(data map[string]any) (ContentBlock, error) {
	blockType, ok := data["type"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid 'type' field")
	}

	switch blockType {
	case BlockTypeText:
		text, _ := data["text"].(string)

		return &TextBlock{Type: BlockTypeText, Text: text}, nil
	case BlockTypeThinking:
		thinking, _ := data["thinking"].(string)
		signature, _ := data["signature"].(string)

		return &ThinkingBlock{Type: BlockTypeThinking, Thinking: thinking, Signature: signature}, nil
	case BlockTypeToolUse:
		id, _ := data["id"].(string)
		name, _ := data["name"].(string)
		input, _ := data["input"].(map[string]any)

		return &ToolUseBlock{Type: BlockTypeToolUse, ID: id, Name: name, Input: input}, nil
	case BlockTypeToolResult:
		block := &ToolResultBlock{Type: BlockTypeToolResult}

		if toolUseID, ok := data["tool_use_id"].(string); ok {
			block.ToolUseID = toolUseID
		}

		if isError, ok := data["is_error"].(bool); ok {
			block.IsError = isError
		}

		if contentData, ok := data["content"].([]any); ok {
			content, err := parseContentBlocks(contentData)
			if err != nil {
				return nil, fmt.Errorf("parse tool result content: %w", err)
			}

			block.Content = content
		}

		return block, nil
	case BlockTypeImage:
		url, _ := data["url"].(string)
		if url == "" {
			url, _ = data["image_url"].(string)
		}

		return &InputImageBlock{Type: BlockTypeImage, URL: url}, nil
	case BlockTypeLocalImage, blockTypeLocalImageWire:
		path, _ := data["path"].(string)

		return &InputLocalImageBlock{Type: BlockTypeLocalImage, Path: path}, nil
	case BlockTypeMention:
		name, _ := data["name"].(string)
		path, _ := data["path"].(string)

		return &InputMentionBlock{Type: BlockTypeMention, Name: name, Path: path}, nil
	default:
		return nil, fmt.Errorf("unknown content block type: %s", blockType)
	}
}
