package codexsdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextResult(t *testing.T) {
	result := TextResult("Hello, World!")

	assert.Len(t, result.Content, 1)
	assert.False(t, result.IsError)

	textContent, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "Hello, World!", textContent.Text)
}

func TestErrorResult(t *testing.T) {
	result := ErrorResult("Something went wrong")

	assert.Len(t, result.Content, 1)
	assert.True(t, result.IsError)

	textContent, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "Something went wrong", textContent.Text)
}

func TestImageResult(t *testing.T) {
	result := ImageResult([]byte("base64data"), "image/png")

	assert.Len(t, result.Content, 1)
	assert.False(t, result.IsError)

	imageContent, ok := result.Content[0].(*mcp.ImageContent)
	require.True(t, ok)
	assert.Equal(t, []byte("base64data"), imageContent.Data)
	assert.Equal(t, "image/png", imageContent.MIMEType)
}

func TestSdkMcpTool(t *testing.T) {
	t.Run("has name and description", func(t *testing.T) {
		tool := NewSdkMcpTool(
			"test_tool",
			"A test tool",
			SimpleSchema(map[string]string{"value": "string"}),
			func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return TextResult("ok"), nil
			},
		)

		assert.Equal(t, "test_tool", tool.Name())
		assert.Equal(t, "A test tool", tool.Description())

		schema := tool.InputSchema()
		assert.NotNil(t, schema)
		assert.Equal(t, "object", schema.Type)
		assert.Contains(t, schema.Properties, "value")
	})

	t.Run("handler executes correctly", func(t *testing.T) {
		tool := NewSdkMcpTool(
			"adder",
			"Adds numbers",
			SimpleSchema(map[string]string{"a": "float64", "b": "float64"}),
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				args, err := ParseArguments(req)
				if err != nil {
					return ErrorResult(err.Error()), nil
				}

				_, _ = args["a"].(float64)
				_, _ = args["b"].(float64)

				return TextResult("Result: sum"), nil
			},
		)

		// Create a mock request
		inputJSON, _ := json.Marshal(map[string]any{"a": 1.0, "b": 2.0})
		req := &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{
				Name:      "adder",
				Arguments: inputJSON,
			},
		}

		result, err := tool.Handler()(context.Background(), req)
		require.NoError(t, err)
		assert.Len(t, result.Content, 1)

		textContent, ok := result.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		assert.Equal(t, "Result: sum", textContent.Text)
	})
}

func TestSimpleSchema(t *testing.T) {
	t.Run("converts simple type map to JSON Schema", func(t *testing.T) {
		schema := SimpleSchema(map[string]string{
			"name":  "string",
			"count": "int",
			"value": "float64",
			"flag":  "bool",
		})

		assert.Equal(t, "object", schema.Type)
		assert.Len(t, schema.Properties, 4)
		assert.Len(t, schema.Required, 4)

		assert.Equal(t, "string", schema.Properties["name"].Type)
		assert.Equal(t, "integer", schema.Properties["count"].Type)
		assert.Equal(t, "number", schema.Properties["value"].Type)
		assert.Equal(t, "boolean", schema.Properties["flag"].Type)
	})

	t.Run("handles array types", func(t *testing.T) {
		schema := SimpleSchema(map[string]string{
			"items": "[]string",
		})

		assert.Equal(t, "array", schema.Properties["items"].Type)
		assert.Equal(t, "string", schema.Properties["items"].Items.Type)
	})
}

func TestParseArguments(t *testing.T) {
	t.Run("parses valid JSON arguments", func(t *testing.T) {
		inputJSON, _ := json.Marshal(map[string]any{"a": 1.0, "b": "hello"})
		req := &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{
				Name:      "test",
				Arguments: inputJSON,
			},
		}

		args, err := ParseArguments(req)
		require.NoError(t, err)
		assert.Equal(t, 1.0, args["a"])
		assert.Equal(t, "hello", args["b"])
	})

	t.Run("handles nil request", func(t *testing.T) {
		args, err := ParseArguments(nil)
		require.NoError(t, err)
		assert.Empty(t, args)
	})

	t.Run("handles empty arguments", func(t *testing.T) {
		req := &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{
				Name:      "test",
				Arguments: nil,
			},
		}

		args, err := ParseArguments(req)
		require.NoError(t, err)
		assert.Empty(t, args)
	})
}

func TestSdkMcpToolWithAnnotations(t *testing.T) {
	annotations := &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		Title:          "My Tool",
	}

	tool := NewSdkMcpTool(
		"annotated_tool",
		"A tool with annotations",
		SimpleSchema(map[string]string{"value": "string"}),
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return TextResult("ok"), nil
		},
		WithAnnotations(annotations),
	)

	assert.Equal(t, "annotated_tool", tool.Name())
	require.NotNil(t, tool.Annotations())
	assert.True(t, tool.Annotations().ReadOnlyHint)
	assert.True(t, tool.Annotations().IdempotentHint)
	assert.Equal(t, "My Tool", tool.Annotations().Title)
}

func TestSdkMcpToolWithoutAnnotations(t *testing.T) {
	tool := NewSdkMcpTool(
		"plain_tool",
		"A tool without annotations",
		SimpleSchema(map[string]string{"value": "string"}),
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return TextResult("ok"), nil
		},
	)

	assert.Nil(t, tool.Annotations())
}

func TestCreateSdkMcpServer(t *testing.T) {
	tool := NewSdkMcpTool(
		"test_tool",
		"A test tool",
		SimpleSchema(map[string]string{"value": "string"}),
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return TextResult("ok"), nil
		},
	)

	config := CreateSdkMcpServer("test_server", "1.0.0", tool)

	assert.Equal(t, MCPServerTypeSDK, config.Type)
	assert.Equal(t, "test_server", config.Name)
	assert.NotNil(t, config.Instance)
}

func TestCreateSdkMcpServerWithAnnotations(t *testing.T) {
	destructive := false

	tool := NewSdkMcpTool(
		"read_data",
		"Read data from source",
		SimpleSchema(map[string]string{"key": "string"}),
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return TextResult("data"), nil
		},
		WithAnnotations(&mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: &destructive,
			IdempotentHint:  true,
		}),
	)

	config := CreateSdkMcpServer("test_server", "1.0.0", tool)
	require.NotNil(t, config.Instance)

	server, ok := config.Instance.(SdkMcpServerInstance)
	require.True(t, ok)

	tools := server.ListTools()
	require.Len(t, tools, 1)

	toolMap := tools[0]
	assert.Equal(t, "read_data", toolMap["name"])

	annotations, ok := toolMap["annotations"].(map[string]any)
	require.True(t, ok, "annotations should be present as map[string]any")
	assert.Equal(t, true, annotations["readOnlyHint"])
	assert.Equal(t, false, annotations["destructiveHint"])
	assert.Equal(t, true, annotations["idempotentHint"])

	// Fields with zero values and no omitempty pointer should still appear
	// but openWorldHint (nil *bool) should be absent
	_, hasOpenWorld := annotations["openWorldHint"]
	assert.False(t, hasOpenWorld, "nil openWorldHint should be omitted")
}

func TestCreateSdkMcpServerWithoutAnnotations(t *testing.T) {
	tool := NewSdkMcpTool(
		"simple_tool",
		"A simple tool",
		SimpleSchema(map[string]string{"value": "string"}),
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return TextResult("ok"), nil
		},
	)

	config := CreateSdkMcpServer("test_server", "1.0.0", tool)
	require.NotNil(t, config.Instance)

	server, ok := config.Instance.(SdkMcpServerInstance)
	require.True(t, ok)

	tools := server.ListTools()
	require.Len(t, tools, 1)

	_, hasAnnotations := tools[0]["annotations"]
	assert.False(t, hasAnnotations, "annotations key should be absent when nil")
}
