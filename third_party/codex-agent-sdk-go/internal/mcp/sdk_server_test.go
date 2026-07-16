package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcpproto "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestNewSDKServer_Metadata(t *testing.T) {
	t.Parallel()

	server := NewSDKServer("calc", "1.0.0")
	require.Equal(t, "calc", server.Name())
	require.Equal(t, "1.0.0", server.Version())
	require.Equal(t, map[string]any{"name": "calc", "version": "1.0.0"}, server.ServerInfo())
	require.Equal(t, map[string]any{"tools": map[string]any{}}, server.Capabilities())
}

func TestSDKServer_ListTools_IncludesSchemaAndAnnotations(t *testing.T) {
	t.Parallel()

	server := NewSDKServer("calc", "1.0.0")
	server.AddTool(&mcpproto.Tool{
		Name:        "add",
		Description: "add two numbers",
		InputSchema: &jsonschema.Schema{
			Type: "object",
		},
		Annotations: &mcpproto.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}, func(_ context.Context, _ *mcpproto.CallToolRequest) (*mcpproto.CallToolResult, error) {
		return TextResult("ok"), nil
	})

	tools := server.ListTools()
	require.Len(t, tools, 1)
	require.Equal(t, "add", tools[0]["name"])
	require.Equal(t, "add two numbers", tools[0]["description"])

	inputSchema, ok := tools[0]["inputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", inputSchema["type"])

	annotations, ok := tools[0]["annotations"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, annotations["readOnlyHint"])
}

func TestSDKServer_CallTool_UnknownTool(t *testing.T) {
	t.Parallel()

	server := NewSDKServer("calc", "1.0.0")
	result, err := server.CallTool(context.Background(), "missing", map[string]any{"a": 1})
	require.NoError(t, err)
	require.Equal(t, true, result["is_error"])

	content, ok := result["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	require.Contains(t, content[0]["text"], "Tool not found")
}

func TestSDKServer_CallTool_MarshalError(t *testing.T) {
	t.Parallel()

	server := NewSDKServer("calc", "1.0.0")
	server.AddTool(NewTool("echo", "echo", nil), func(
		_ context.Context,
		_ *mcpproto.CallToolRequest,
	) (*mcpproto.CallToolResult, error) {
		return TextResult("ok"), nil
	})

	result, err := server.CallTool(context.Background(), "echo", map[string]any{
		"bad": func() {},
	})
	require.NoError(t, err)
	require.Equal(t, true, result["is_error"])

	content, ok := result["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	require.Contains(t, content[0]["text"], "Failed to marshal input")
}

func TestSDKServer_CallTool_HandlerError(t *testing.T) {
	t.Parallel()

	server := NewSDKServer("calc", "1.0.0")
	server.AddTool(NewTool("echo", "echo", nil), func(
		_ context.Context,
		_ *mcpproto.CallToolRequest,
	) (*mcpproto.CallToolResult, error) {
		return nil, errors.New("boom")
	})

	result, err := server.CallTool(context.Background(), "echo", map[string]any{"x": 1})
	require.NoError(t, err)
	require.Equal(t, true, result["is_error"])

	content, ok := result["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	require.Contains(t, content[0]["text"], "Tool execution failed: boom")
}

func TestSDKServer_CallTool_Success(t *testing.T) {
	t.Parallel()

	server := NewSDKServer("calc", "1.0.0")
	server.AddTool(NewTool("echo", "echo", nil), func(
		_ context.Context,
		req *mcpproto.CallToolRequest,
	) (*mcpproto.CallToolResult, error) {
		args, err := ParseArguments(req)
		require.NoError(t, err)
		require.Equal(t, "world", args["name"])

		return TextResult("hello world"), nil
	})

	result, err := server.CallTool(context.Background(), "echo", map[string]any{"name": "world"})
	require.NoError(t, err)
	require.NotContains(t, result, "is_error")

	content, ok := result["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	require.Equal(t, map[string]any{"type": "text", "text": "hello world"}, content[0])
}

func TestConvertCallToolResultToMap(t *testing.T) {
	t.Parallel()

	require.Equal(t, map[string]any{"content": []map[string]any{}}, convertCallToolResultToMap(nil))

	result := &mcpproto.CallToolResult{
		Content: []mcpproto.Content{
			&mcpproto.TextContent{Text: "hello"},
			&mcpproto.ImageContent{Data: []byte("image-data"), MIMEType: "image/png"},
			&mcpproto.AudioContent{Data: []byte("audio-data"), MIMEType: "audio/wav"},
			&mcpproto.ResourceLink{URI: "file:///tmp/a.txt", Name: "a"},
			&mcpproto.EmbeddedResource{
				Resource: &mcpproto.ResourceContents{
					URI:      "file:///tmp/b.txt",
					MIMEType: "text/plain",
					Text:     "body",
				},
			},
			&mcpproto.EmbeddedResource{},
		},
		IsError: true,
	}

	got := convertCallToolResultToMap(result)
	require.Equal(t, true, got["is_error"])

	content, ok := got["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, content, 5)
	require.Equal(t, map[string]any{"type": "text", "text": "hello"}, content[0])
	require.Equal(t, "image", content[1]["type"])
	require.Equal(t, "audio", content[2]["type"])
	require.Equal(t, "resource_link", content[3]["type"])
	require.Equal(t, "resource", content[4]["type"])

	resource, ok := content[4]["resource"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "file:///tmp/b.txt", resource["uri"])
	require.Equal(t, "text/plain", resource["mimeType"])
	require.Equal(t, "body", resource["text"])
}

func TestSimpleSchema(t *testing.T) {
	t.Parallel()

	schema := SimpleSchema(map[string]string{
		"name":   "string",
		"count":  "int",
		"values": "[]float64",
	})

	require.Equal(t, "object", schema.Type)
	require.ElementsMatch(t, []string{"name", "count", "values"}, schema.Required)
	require.Equal(t, "string", schema.Properties["name"].Type)
	require.Equal(t, "integer", schema.Properties["count"].Type)
	require.Equal(t, "array", schema.Properties["values"].Type)
	require.Equal(t, "number", schema.Properties["values"].Items.Type)
}

func TestGoTypeToJSONSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		goType string
		want   string
	}{
		{name: "string", goType: "string", want: "string"},
		{name: "integer", goType: "int64", want: "integer"},
		{name: "number", goType: "float64", want: "number"},
		{name: "boolean", goType: "bool", want: "boolean"},
		{name: "object", goType: "map[string]any", want: "object"},
		{name: "array", goType: "[]string", want: "array"},
		{name: "fallback", goType: "custom", want: "string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := goTypeToJSONSchema(tt.goType)
			require.Equal(t, tt.want, got.Type)
		})
	}
}

func TestParseArguments(t *testing.T) {
	t.Parallel()

	t.Run("nil request", func(t *testing.T) {
		t.Parallel()

		args, err := ParseArguments(nil)
		require.NoError(t, err)
		require.Empty(t, args)
	})

	t.Run("empty arguments", func(t *testing.T) {
		t.Parallel()

		args, err := ParseArguments(&mcpproto.CallToolRequest{
			Params: &mcpproto.CallToolParamsRaw{
				Name: "echo",
			},
		})
		require.NoError(t, err)
		require.Empty(t, args)
	})

	t.Run("valid arguments", func(t *testing.T) {
		t.Parallel()

		args, err := ParseArguments(&mcpproto.CallToolRequest{
			Params: &mcpproto.CallToolParamsRaw{
				Name:      "echo",
				Arguments: json.RawMessage(`{"name":"world","count":2}`),
			},
		})
		require.NoError(t, err)
		require.Equal(t, "world", args["name"])
		require.Equal(t, float64(2), args["count"])
	})

	t.Run("invalid arguments", func(t *testing.T) {
		t.Parallel()

		_, err := ParseArguments(&mcpproto.CallToolRequest{
			Params: &mcpproto.CallToolParamsRaw{
				Name:      "echo",
				Arguments: json.RawMessage(`{invalid}`),
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to unmarshal arguments")
	})
}

func TestResultConstructorsAndNewTool(t *testing.T) {
	t.Parallel()

	text := TextResult("ok")
	require.False(t, text.IsError)
	require.Len(t, text.Content, 1)
	textContent, ok := text.Content[0].(*mcpproto.TextContent)
	require.True(t, ok)
	require.Equal(t, "ok", textContent.Text)

	errResult := ErrorResult("nope")
	require.True(t, errResult.IsError)
	require.Len(t, errResult.Content, 1)
	errContent, ok := errResult.Content[0].(*mcpproto.TextContent)
	require.True(t, ok)
	require.Equal(t, "nope", errContent.Text)

	imgResult := ImageResult([]byte("image"), "image/png")
	require.False(t, imgResult.IsError)
	require.Len(t, imgResult.Content, 1)
	imgContent, ok := imgResult.Content[0].(*mcpproto.ImageContent)
	require.True(t, ok)
	require.Equal(t, []byte("image"), imgContent.Data)
	require.Equal(t, "image/png", imgContent.MIMEType)

	schema := &jsonschema.Schema{Type: "object"}
	tool := NewTool("lookup", "lookup value", schema)
	require.Equal(t, "lookup", tool.Name)
	require.Equal(t, "lookup value", tool.Description)
	require.Equal(t, schema, tool.InputSchema)
}
