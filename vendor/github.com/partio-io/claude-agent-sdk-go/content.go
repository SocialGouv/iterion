package claude

import "encoding/json"

// ContentBlock is a sealed interface for content within a message.
// Concrete types: [TextBlock], [ThinkingBlock], [ToolUseBlock], [ToolResultBlock].
type ContentBlock interface {
	blockType() string
	sealedBlock()
}

// TextBlock represents a text content block.
type TextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

func (*TextBlock) blockType() string { return "text" }
func (*TextBlock) sealedBlock()      {}

// ThinkingBlock represents an extended thinking content block.
type ThinkingBlock struct {
	Type      string `json:"type"` // "thinking"
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

func (*ThinkingBlock) blockType() string { return "thinking" }
func (*ThinkingBlock) sealedBlock()      {}

// ToolUseBlock represents a tool invocation by Claude.
type ToolUseBlock struct {
	Type  string         `json:"type"` // "tool_use"
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func (*ToolUseBlock) blockType() string { return "tool_use" }
func (*ToolUseBlock) sealedBlock()      {}

// ToolResultBlock represents the result of a tool invocation.
type ToolResultBlock struct {
	Type      string `json:"type"` // "tool_result"
	ToolUseID string `json:"tool_use_id"`
	Content   any    `json:"content"` // string or []ContentBlock
	IsError   bool   `json:"is_error"`
}

func (*ToolResultBlock) blockType() string { return "tool_result" }
func (*ToolResultBlock) sealedBlock()      {}

// unmarshalContentBlock deserializes a JSON content block into the appropriate concrete type.
func unmarshalContentBlock(raw json.RawMessage) (ContentBlock, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case "text":
		var b TextBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		return &b, nil
	case "thinking":
		var b ThinkingBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		return &b, nil
	case "tool_use":
		var b ToolUseBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		return &b, nil
	case "tool_result":
		var b ToolResultBlock
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		return &b, nil
	default:
		// Unknown block type — return as text with raw JSON.
		return &TextBlock{Type: probe.Type, Text: string(raw)}, nil
	}
}

// unmarshalContentBlocks deserializes a JSON array of content blocks.
func unmarshalContentBlocks(raw json.RawMessage) ([]ContentBlock, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, err
	}
	blocks := make([]ContentBlock, 0, len(raws))
	for _, r := range raws {
		b, err := unmarshalContentBlock(r)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}
