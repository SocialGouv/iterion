package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time verification that SDKServer implements ServerInstance.
var _ ServerInstance = (*SDKServer)(nil)

// SDKServer wraps the official MCP SDK server for programmatic access.
type SDKServer struct {
	name    string
	version string
	mu      sync.RWMutex
	tools   map[string]*sdkTool
}

// sdkTool holds tool metadata and handler for internal registry.
type sdkTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
}

// NewSDKServer creates a new MCP SDK server wrapper.
func NewSDKServer(name, version string) *SDKServer {
	return &SDKServer{
		name:    name,
		version: version,
		tools:   make(map[string]*sdkTool, 8),
	}
}

// AddTool registers a tool with the server.
func (s *SDKServer) AddTool(tool *mcp.Tool, handler mcp.ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tools[tool.Name] = &sdkTool{
		tool:    tool,
		handler: handler,
	}
}

// Name returns the server name.
func (s *SDKServer) Name() string { return s.name }

// Version returns the server version.
func (s *SDKServer) Version() string { return s.version }

// ServerInfo returns server information for MCP initialize response.
func (s *SDKServer) ServerInfo() map[string]any {
	return map[string]any{
		"name":    s.name,
		"version": s.version,
	}
}

// Capabilities returns server capabilities for MCP initialize response.
func (s *SDKServer) Capabilities() map[string]any {
	return map[string]any{
		"tools": map[string]any{},
	}
}

// ListTools returns metadata for all registered tools.
func (s *SDKServer) ListTools() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		toolMap := map[string]any{
			"name":        t.tool.Name,
			"description": t.tool.Description,
		}

		if t.tool.InputSchema != nil {
			schemaData, err := json.Marshal(t.tool.InputSchema)
			if err == nil {
				var schemaMap map[string]any
				if json.Unmarshal(schemaData, &schemaMap) == nil {
					toolMap["inputSchema"] = schemaMap
				}
			}
		}

		if t.tool.Annotations != nil {
			annotData, err := json.Marshal(t.tool.Annotations)
			if err == nil {
				var annotMap map[string]any
				if json.Unmarshal(annotData, &annotMap) == nil {
					toolMap["annotations"] = annotMap
				}
			}
		}

		result = append(result, toolMap)
	}

	return result
}

// CallTool executes a tool by name with the given input.
func (s *SDKServer) CallTool(
	ctx context.Context,
	name string,
	input map[string]any,
) (map[string]any, error) {
	s.mu.RLock()
	t, exists := s.tools[name]
	s.mu.RUnlock()

	if !exists {
		return map[string]any{
			"content":  []map[string]any{{"type": "text", "text": "Tool not found: " + name}},
			"is_error": true,
		}, nil
	}

	inputBytes, err := json.Marshal(input)
	if err != nil {
		//nolint:nilerr // Error is encoded in the result
		return map[string]any{
			"content":  []map[string]any{{"type": "text", "text": "Failed to marshal input: " + err.Error()}},
			"is_error": true,
		}, nil
	}

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      name,
			Arguments: inputBytes,
		},
	}

	result, err := t.handler(ctx, req)
	if err != nil {
		//nolint:nilerr // Error is encoded in the result
		return map[string]any{
			"content":  []map[string]any{{"type": "text", "text": "Tool execution failed: " + err.Error()}},
			"is_error": true,
		}, nil
	}

	return convertCallToolResultToMap(result), nil
}

// convertCallToolResultToMap converts an MCP CallToolResult to a map.
func convertCallToolResultToMap(result *mcp.CallToolResult) map[string]any {
	if result == nil {
		return map[string]any{
			"content": []map[string]any{},
		}
	}

	content := make([]map[string]any, 0, len(result.Content))
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			content = append(content, map[string]any{
				"type": "text",
				"text": v.Text,
			})
		case *mcp.ImageContent:
			content = append(content, map[string]any{
				"type":     "image",
				"data":     v.Data,
				"mimeType": v.MIMEType,
			})
		case *mcp.AudioContent:
			content = append(content, map[string]any{
				"type":     "audio",
				"data":     v.Data,
				"mimeType": v.MIMEType,
			})
		case *mcp.ResourceLink:
			content = append(content, map[string]any{
				"type": "resource_link",
				"uri":  v.URI,
				"name": v.Name,
			})
		case *mcp.EmbeddedResource:
			if v.Resource != nil {
				content = append(content, map[string]any{
					"type": "resource",
					"resource": map[string]any{
						"uri":      v.Resource.URI,
						"mimeType": v.Resource.MIMEType,
						"text":     v.Resource.Text,
					},
				})
			}
		}
	}

	resultMap := map[string]any{
		"content": content,
	}

	if result.IsError {
		resultMap["is_error"] = true
	}

	return resultMap
}

// SimpleSchema creates a jsonschema.Schema from a simple type map.
func SimpleSchema(props map[string]string) *jsonschema.Schema {
	properties := make(map[string]*jsonschema.Schema, len(props))
	required := make([]string, 0, len(props))

	for name, goType := range props {
		properties[name] = goTypeToJSONSchema(goType)
		required = append(required, name)
	}

	return &jsonschema.Schema{
		Type:       "object",
		Properties: properties,
		Required:   required,
	}
}

// goTypeToJSONSchema converts a Go type string to a JSON Schema type.
func goTypeToJSONSchema(goType string) *jsonschema.Schema {
	switch goType {
	case "string":
		return &jsonschema.Schema{Type: "string"}
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		return &jsonschema.Schema{Type: "integer"}
	case "float32", "float64", "float", "number":
		return &jsonschema.Schema{Type: "number"}
	case "bool", "boolean":
		return &jsonschema.Schema{Type: "boolean"}
	case "any", "object", "map[string]any":
		return &jsonschema.Schema{Type: "object"}
	default:
		if len(goType) > 2 && goType[:2] == "[]" {
			return &jsonschema.Schema{
				Type:  "array",
				Items: goTypeToJSONSchema(goType[2:]),
			}
		}

		return &jsonschema.Schema{Type: "string"}
	}
}

// TextResult creates a CallToolResult with text content.
func TextResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}
}

// ErrorResult creates a CallToolResult indicating an error.
func ErrorResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: message},
		},
		IsError: true,
	}
}

// ImageResult creates a CallToolResult with image content.
func ImageResult(data []byte, mimeType string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{Data: data, MIMEType: mimeType},
		},
	}
}

// NewTool creates an mcp.Tool with the given parameters.
func NewTool(name, description string, inputSchema *jsonschema.Schema) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
	}
}

// ParseArguments unmarshals CallToolRequest arguments into a map.
func ParseArguments(req *mcp.CallToolRequest) (map[string]any, error) {
	if req == nil || req.Params == nil {
		return make(map[string]any), nil
	}

	if len(req.Params.Arguments) == 0 {
		return make(map[string]any), nil
	}

	var args map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return nil, fmt.Errorf("failed to unmarshal arguments: %w", err)
	}

	return args, nil
}
