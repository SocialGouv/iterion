package protocol

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/elicitation"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/permission"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/userinput"
	"github.com/stretchr/testify/require"
)

type testSDKMCPServer struct {
	name string
}

func (s *testSDKMCPServer) Name() string { return s.name }

func (s *testSDKMCPServer) Version() string { return "1.0.0" }

func (s *testSDKMCPServer) ListTools() []map[string]any {
	return []map[string]any{{
		"name": "add",
	}}
}

func (s *testSDKMCPServer) CallTool(_ context.Context, name string, _ map[string]any) (map[string]any, error) {
	return nil, fmt.Errorf("unexpected CallTool(%s)", name)
}

// TestSession_NeedsInitialization_Empty tests that NeedsInitialization returns false
// when no CanUseTool or MCP servers are configured.
func TestSession_NeedsInitialization_Empty(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log:             log,
		options:         &config.Options{},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	require.False(t, session.NeedsInitialization(),
		"Expected NeedsInitialization() to return false with empty options")
}

func TestSession_NeedsInitialization_AdvancedOptionsAlone(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log: log,
		options: &config.Options{
			Resume:               "thread_123",
			ContinueConversation: true,
			OutputFormat:         map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}},
		},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	require.False(t, session.NeedsInitialization(),
		"Expected NeedsInitialization() to remain false without callbacks/MCP")
}

func TestSession_NeedsInitialization_WithDynamicTools(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log:           log,
		options:       &config.Options{},
		sdkMcpServers: make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: map[string]*config.DynamicTool{
			"add": {Name: "add", Description: "Add numbers"},
		},
	}

	require.True(t, session.NeedsInitialization(),
		"Expected NeedsInitialization() to return true with dynamic tools")
}

func TestSession_RegisterDynamicTools(t *testing.T) {
	log := slog.Default()

	tools := []*config.DynamicTool{
		{
			Name:        "add",
			Description: "Add two numbers",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return map[string]any{"result": 42}, nil
			},
		},
		{
			Name:        "multiply",
			Description: "Multiply two numbers",
			Handler: func(_ context.Context, _ map[string]any) (map[string]any, error) {
				return map[string]any{"result": 6}, nil
			},
		},
	}

	session := NewSession(log, nil, &config.Options{SDKTools: tools})
	session.RegisterDynamicTools()

	require.Len(t, session.sdkDynamicTools, 2)
	require.NotNil(t, session.sdkDynamicTools["add"])
	require.NotNil(t, session.sdkDynamicTools["multiply"])
	require.Equal(t, "Add two numbers", session.sdkDynamicTools["add"].Description)
}

func TestSession_BuildInitializePayload_DynamicTools(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log: log,
		options: &config.Options{
			SDKTools: []*config.DynamicTool{
				{
					Name:        "add",
					Description: "Add two numbers",
					InputSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"a": map[string]any{"type": "number"},
							"b": map[string]any{"type": "number"},
						},
						"required": []string{"a", "b"},
					},
				},
				{
					Name:        "greet",
					Description: "Say hello",
				},
			},
		},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	payload := session.buildInitializePayload()

	dynamicTools, ok := payload["dynamicTools"].([]map[string]any)
	require.True(t, ok, "dynamicTools should be []map[string]any")
	require.Len(t, dynamicTools, 2)

	require.Equal(t, "add", dynamicTools[0]["name"])
	require.Equal(t, "Add two numbers", dynamicTools[0]["description"])
	require.NotNil(t, dynamicTools[0]["inputSchema"])

	inputSchema, ok := dynamicTools[0]["inputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", inputSchema["type"])

	require.Equal(t, "greet", dynamicTools[1]["name"])
	require.Equal(t, "Say hello", dynamicTools[1]["description"])
	require.Nil(t, dynamicTools[1]["inputSchema"])
}

func TestSession_HandleDynamicToolCall_PlainName(t *testing.T) {
	log := slog.Default()

	var calledWith map[string]any

	session := NewSession(log, nil, &config.Options{})
	session.sdkDynamicTools["add"] = &config.DynamicTool{
		Name: "add",
		Handler: func(_ context.Context, input map[string]any) (map[string]any, error) {
			calledWith = input

			return map[string]any{"result": 42}, nil
		},
	}

	resp, err := session.HandleDynamicToolCall(context.Background(), &ControlRequest{
		Request: map[string]any{
			"tool":      "add",
			"arguments": map[string]any{"a": float64(5), "b": float64(3)},
		},
	})

	require.NoError(t, err)
	require.Equal(t, true, resp["success"])
	require.NotNil(t, calledWith)
	require.Equal(t, float64(5), calledWith["a"])

	items, ok := resp["contentItems"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	require.Contains(t, items[0]["text"], "42")
}

func TestSession_HandleDynamicToolCall_FallbackMCP(t *testing.T) {
	log := slog.Default()

	session := NewSession(log, nil, &config.Options{})
	// No dynamic tools registered, but we have an MCP server
	// The MCP fallback should try to parse the name and fail
	// since no MCP server is registered.

	resp, err := session.HandleDynamicToolCall(context.Background(), &ControlRequest{
		Request: map[string]any{
			"tool":      "mcp__sdk__calc",
			"arguments": map[string]any{},
		},
	})

	require.NoError(t, err)
	require.Equal(t, false, resp["success"])

	items, ok := resp["contentItems"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	require.Contains(t, items[0]["text"], "SDK MCP server not found")
}

func TestSession_HandleDynamicToolCall_UnknownTool(t *testing.T) {
	log := slog.Default()

	session := NewSession(log, nil, &config.Options{})

	resp, err := session.HandleDynamicToolCall(context.Background(), &ControlRequest{
		Request: map[string]any{
			"tool":      "nonexistent",
			"arguments": map[string]any{},
		},
	})

	require.NoError(t, err)
	require.Equal(t, false, resp["success"])

	items, ok := resp["contentItems"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	require.Contains(t, items[0]["text"], "unknown tool")
}

func TestSession_HandleCanUseTool_PublicSDKMCPNameAllowed(t *testing.T) {
	log := slog.Default()

	opts := &config.Options{
		AllowedTools: []string{"mcp__calc__add"},
	}
	require.NoError(t, config.ConfigureToolPermissionPolicy(opts))

	session := NewSession(log, nil, opts)
	session.sdkMcpServers["calc"] = &testSDKMCPServer{name: "calc"}

	resp, err := session.HandleCanUseTool(context.Background(), &ControlRequest{
		Request: map[string]any{
			"tool_name": "sdkmcp__calc__add",
			"input": map[string]any{
				"a": float64(15),
				"b": float64(27),
			},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["decision"])
}

func TestSession_HandleCanUseTool_PlainDynamicSDKMCPPrefixPreserved(t *testing.T) {
	log := slog.Default()

	opts := &config.Options{
		AllowedTools: []string{"sdkmcp__plain_dynamic_tool"},
	}
	require.NoError(t, config.ConfigureToolPermissionPolicy(opts))

	session := NewSession(log, nil, opts)

	resp, err := session.HandleCanUseTool(context.Background(), &ControlRequest{
		Request: map[string]any{
			"tool_name": "sdkmcp__plain_dynamic_tool",
			"input": map[string]any{
				"value": "secret",
			},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["decision"])
}

func TestSession_BuildInitializePayload_IncludesAdvancedFields(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log: log,
		options: &config.Options{
			Model:                "gpt-5.4",
			Cwd:                  "/tmp/project",
			ContinueConversation: true,
			Resume:               "thread_abc",
			ForkSession:          true,
			AddDirs:              []string{"/tmp/extra"},
			OutputFormat: map[string]any{
				"type": "json_schema",
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"answer": map[string]any{"type": "string"},
					},
				},
			},
		},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	payload := session.buildInitializePayload()

	require.Equal(t, "gpt-5.4", payload["model"])
	require.Equal(t, "/tmp/project", payload["cwd"])
	require.Equal(t, true, payload["continueConversation"])
	require.Equal(t, "thread_abc", payload["resume"])
	require.Equal(t, true, payload["forkSession"])
	require.Equal(t, []string{"/tmp/extra"}, payload["addDirs"])

	outputSchema, ok := payload["outputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", outputSchema["type"])
}

// TestSession_InitializationResult_DataRace tests for data race between
// writing initializationResult and reading it via GetInitializationResult().
// Run with: go test -race -run TestSession_InitializationResult_DataRace.
func TestSession_InitializationResult_DataRace(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log:             log,
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	const iterations = 1000

	var wg sync.WaitGroup

	// Writer goroutine: simulates what Initialize() does (with mutex protection)

	wg.Go(func() {
		for i := range iterations {
			session.initMu.Lock()
			session.initializationResult = map[string]any{
				"iteration": i,
				"data":      "test",
			}
			session.initMu.Unlock()
		}
	})

	// Reader goroutine: simulates concurrent GetInitializationResult() calls

	wg.Go(func() {
		for range iterations {
			result := session.GetInitializationResult()

			// Access the map to ensure the race detector catches any issues
			if result != nil {
				_ = len(result)
			}
		}
	})

	wg.Wait()
}

// TestSession_InitializationResult_ConcurrentReadWrite tests the race between
// a single write and multiple concurrent reads.
// Run with: go test -race -run TestSession_InitializationResult_ConcurrentReadWrite.
func TestSession_InitializationResult_ConcurrentReadWrite(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log:             log,
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	const (
		readers    = 10
		iterations = 1000
	)

	var wg sync.WaitGroup

	// Single writer (simulates Initialize with mutex protection)

	wg.Go(func() {
		for i := range iterations {
			session.initMu.Lock()
			session.initializationResult = map[string]any{
				"version": "1.0.0",
				"count":   i,
			}
			session.initMu.Unlock()
		}
	})

	// Multiple readers using GetInitializationResult()
	for range readers {
		wg.Go(func() {
			for range iterations {
				result := session.GetInitializationResult()
				if result != nil {
					// Access map contents - safe because we received a copy
					_ = result["version"]
					_ = result["count"]
				}
			}
		})
	}

	wg.Wait()
}

func TestSession_HandleRequestUserInput_NoCallback(t *testing.T) {
	log := slog.Default()

	session := NewSession(log, nil, &config.Options{})

	resp, err := session.HandleRequestUserInput(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":   "item_tool/requestUserInput",
			"item_id":   "item_1",
			"thread_id": "thread_1",
			"questions": []any{
				map[string]any{
					"id":       "q1",
					"question": "Pick a language",
					"options": []any{
						map[string]any{"label": "Go", "description": "Fast compiled"},
					},
				},
			},
		},
	})

	require.NoError(t, err)

	answers, ok := resp["answers"].(map[string]any)
	require.True(t, ok)
	require.Empty(t, answers, "no callback should return empty answers")
}

func TestSession_HandleRequestUserInput_WithCallback(t *testing.T) {
	log := slog.Default()

	var captured *userinput.Request

	session := NewSession(log, nil, &config.Options{
		OnUserInput: func(_ context.Context, req *userinput.Request) (*userinput.Response, error) {
			captured = req

			answers := make(map[string]*userinput.Answer, 1)
			answers[req.Questions[0].ID] = &userinput.Answer{
				Answers: []string{req.Questions[0].Options[1].Label},
			}

			return &userinput.Response{Answers: answers}, nil
		},
	})

	resp, err := session.HandleRequestUserInput(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":   "item_tool/requestUserInput",
			"item_id":   "item_42",
			"thread_id": "thread_7",
			"turn_id":   "turn_3",
			"questions": []any{
				map[string]any{
					"id":           "lang",
					"header":       "Language",
					"question":     "Which language?",
					"multi_select": true,
					"options": []any{
						map[string]any{"label": "Go", "description": "Fast compiled"},
						map[string]any{"label": "Rust", "description": "Memory safe"},
						map[string]any{"label": "Python", "description": "Interpreted"},
					},
				},
			},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, captured)
	require.Equal(t, "item_42", captured.ItemID)
	require.Equal(t, "thread_7", captured.ThreadID)
	require.Equal(t, "turn_3", captured.TurnID)
	require.Len(t, captured.Questions, 1)
	require.Equal(t, "lang", captured.Questions[0].ID)
	require.Equal(t, "Language", captured.Questions[0].Header)
	require.Equal(t, "Which language?", captured.Questions[0].Question)
	require.True(t, captured.Questions[0].MultiSelect)
	require.Len(t, captured.Questions[0].Options, 3)
	require.Equal(t, "Rust", captured.Questions[0].Options[1].Label)

	answers, ok := resp["answers"].(map[string]any)
	require.True(t, ok)

	langAnswer, ok := answers["lang"].(map[string]any)
	require.True(t, ok)

	answerList, ok := langAnswer["answers"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{"Rust"}, answerList)
}

func TestSession_HandleRequestUserInput_FreeText(t *testing.T) {
	log := slog.Default()

	session := NewSession(log, nil, &config.Options{
		OnUserInput: func(_ context.Context, req *userinput.Request) (*userinput.Response, error) {
			answers := make(map[string]*userinput.Answer, 1)
			answers[req.Questions[0].ID] = &userinput.Answer{
				Answers: []string{"my free text answer"},
			}

			return &userinput.Response{Answers: answers}, nil
		},
	})

	resp, err := session.HandleRequestUserInput(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":   "item_tool/requestUserInput",
			"item_id":   "item_1",
			"thread_id": "thread_1",
			"questions": []any{
				map[string]any{
					"id":       "name",
					"question": "What is your name?",
				},
			},
		},
	})

	require.NoError(t, err)

	answers, ok := resp["answers"].(map[string]any)
	require.True(t, ok)

	nameAnswer, ok := answers["name"].(map[string]any)
	require.True(t, ok)

	answerList, ok := nameAnswer["answers"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{"my free text answer"}, answerList)
}

func TestSession_NeedsInitialization_WithOnUserInput(t *testing.T) {
	log := slog.Default()

	session := &Session{
		log: log,
		options: &config.Options{
			OnUserInput: func(_ context.Context, _ *userinput.Request) (*userinput.Response, error) {
				return &userinput.Response{}, nil
			},
		},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	require.True(t, session.NeedsInitialization(),
		"Expected NeedsInitialization() to return true with OnUserInput set")
}

func TestMapPermissionToApprovalPolicy_Plan(t *testing.T) {
	require.Equal(t, "on-request", mapPermissionToApprovalPolicy("plan"))
}

func TestSession_BuildInitializePayload_Personality(t *testing.T) {
	session := &Session{
		log:             slog.Default(),
		options:         &config.Options{Personality: "pragmatic"},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	payload := session.buildInitializePayload()
	require.Equal(t, "pragmatic", payload["personality"])
}

func TestSession_BuildInitializePayload_ServiceTier(t *testing.T) {
	session := &Session{
		log:             slog.Default(),
		options:         &config.Options{ServiceTier: "fast"},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	payload := session.buildInitializePayload()
	require.Equal(t, "fast", payload["serviceTier"])
}

func TestSession_BuildInitializePayload_DeveloperInstructions(t *testing.T) {
	session := &Session{
		log:             slog.Default(),
		options:         &config.Options{DeveloperInstructions: "Always respond in JSON"},
		sdkMcpServers:   make(map[string]mcp.ServerInstance, 4),
		sdkDynamicTools: make(map[string]*config.DynamicTool, 4),
	}

	payload := session.buildInitializePayload()
	require.Equal(t, "Always respond in JSON", payload["developerInstructions"])
}

func TestSession_HandleFileChangeApproval_NoCallback(t *testing.T) {
	session := NewSession(slog.Default(), nil, &config.Options{})

	resp, err := session.HandleFileChangeApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":  "item_fileChange/requestApproval",
			"itemId":   "item_1",
			"threadId": "thread_1",
			"turnId":   "turn_1",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["decision"])
}

func TestSession_HandleFileChangeApproval_WithCallback(t *testing.T) {
	var (
		capturedTool  string
		capturedInput map[string]any
	)

	opts := &config.Options{
		CanUseTool: func(_ context.Context, toolName string, input map[string]any, _ *permission.Context) (permission.Result, error) {
			capturedTool = toolName
			capturedInput = input

			return &permission.ResultDeny{}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleFileChangeApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":   "item_fileChange/requestApproval",
			"itemId":    "item_42",
			"threadId":  "thread_1",
			"turnId":    "turn_1",
			"grantRoot": "/tmp/project",
			"reason":    "needs write access",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "decline", resp["decision"])
	require.Equal(t, "Edit", capturedTool)
	require.Equal(t, "item_42", capturedInput["itemId"])
	require.Equal(t, "/tmp/project", capturedInput["grantRoot"])
	require.Equal(t, "needs write access", capturedInput["reason"])
}

func TestSession_HandleFileChangeApproval_AllowedWritePolicyAcceptsEditApproval(t *testing.T) {
	opts := &config.Options{
		AllowedTools: []string{"Write"},
	}
	require.NoError(t, config.ConfigureToolPermissionPolicy(opts))

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleFileChangeApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype": "item_fileChange/requestApproval",
			"itemId":  "item_create",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["decision"])
}

func TestSession_HandleFileChangeApproval_LegacyApplyPatchUsesWriteForAdds(t *testing.T) {
	var capturedTool string

	opts := &config.Options{
		CanUseTool: func(_ context.Context, toolName string, _ map[string]any, _ *permission.Context) (permission.Result, error) {
			capturedTool = toolName

			return &permission.ResultAllow{}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleFileChangeApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype": "applyPatchApproval",
			"fileChanges": map[string]any{
				"/tmp/new.txt": map[string]any{
					"type":    "add",
					"content": "hello",
				},
			},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["decision"])
	require.Equal(t, "Write", capturedTool)
}

func TestSession_HandlePermissionsApproval_NoCallback(t *testing.T) {
	session := NewSession(slog.Default(), nil, &config.Options{})

	reqPerms := map[string]any{
		"fileSystem": map[string]any{"write": []any{"/tmp"}},
	}

	resp, err := session.HandlePermissionsApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":     "item_permissions/requestApproval",
			"itemId":      "item_1",
			"threadId":    "thread_1",
			"turnId":      "turn_1",
			"permissions": reqPerms,
		},
	})

	require.NoError(t, err)
	require.Equal(t, reqPerms, resp["permissions"])
	require.Equal(t, "turn", resp["scope"])
}

func TestSession_HandlePermissionsApproval_WithCallback(t *testing.T) {
	var (
		capturedTool  string
		capturedInput map[string]any
	)

	opts := &config.Options{
		CanUseTool: func(_ context.Context, toolName string, input map[string]any, _ *permission.Context) (permission.Result, error) {
			capturedTool = toolName
			capturedInput = input

			return &permission.ResultAllow{}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	reqPerms := map[string]any{
		"network": map[string]any{"enabled": true},
	}

	resp, err := session.HandlePermissionsApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":     "item_permissions/requestApproval",
			"itemId":      "item_1",
			"threadId":    "thread_1",
			"turnId":      "turn_1",
			"permissions": reqPerms,
			"reason":      "need network",
		},
	})

	require.NoError(t, err)
	require.Equal(t, reqPerms, resp["permissions"])
	require.Equal(t, "turn", resp["scope"])
	require.Equal(t, "Permissions", capturedTool)
	require.NotNil(t, capturedInput["permissions"])
	require.Equal(t, "need network", capturedInput["reason"])
}

func TestSession_HandlePermissionsApproval_AllowedWritePolicyBypassesFilter(t *testing.T) {
	opts := &config.Options{
		AllowedTools: []string{"Write"},
	}
	require.NoError(t, config.ConfigureToolPermissionPolicy(opts))

	session := NewSession(slog.Default(), nil, opts)

	reqPerms := map[string]any{
		"fileSystem": map[string]any{"write": []any{"/tmp"}},
	}

	resp, err := session.HandlePermissionsApproval(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":     "item_permissions/requestApproval",
			"permissions": reqPerms,
		},
	})

	require.NoError(t, err)
	require.Equal(t, reqPerms, resp["permissions"])
	require.Equal(t, "turn", resp["scope"])
}

func TestSession_HandleMCPElicitation_NoCallback(t *testing.T) {
	session := NewSession(slog.Default(), nil, &config.Options{})

	resp, err := session.HandleMCPElicitation(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype": "mcpServer_elicitation/request",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "decline", resp["action"])
}

func TestSession_HandleMCPElicitation_WithCallback(t *testing.T) {
	var capturedReq *elicitation.Request

	opts := &config.Options{
		OnElicitation: func(_ context.Context, req *elicitation.Request) (*elicitation.Response, error) {
			capturedReq = req

			return &elicitation.Response{
				Action:  elicitation.ActionAccept,
				Content: map[string]any{"name": "test-value"},
			}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleMCPElicitation(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":    "mcpServer_elicitation/request",
			"serverName": "my-mcp-server",
			"message":    "Please provide credentials",
			"mode":       "form",
			"threadId":   "thread-1",
			"turnId":     "turn-1",
			"requestedSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
			},
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["action"])
	require.Equal(t, map[string]any{"name": "test-value"}, resp["content"])

	require.NotNil(t, capturedReq)
	require.Equal(t, "my-mcp-server", capturedReq.MCPServerName)
	require.Equal(t, "Please provide credentials", capturedReq.Message)
	require.Equal(t, "thread-1", capturedReq.ThreadID)

	require.NotNil(t, capturedReq.Mode)
	require.Equal(t, elicitation.ModeForm, *capturedReq.Mode)

	require.NotNil(t, capturedReq.TurnID)
	require.Equal(t, "turn-1", *capturedReq.TurnID)

	require.NotNil(t, capturedReq.RequestedSchema)
	require.Equal(t, "object", capturedReq.RequestedSchema["type"])

	require.NotNil(t, capturedReq.Audit)
}

func TestSession_HandleMCPElicitation_URLMode(t *testing.T) {
	opts := &config.Options{
		OnElicitation: func(_ context.Context, req *elicitation.Request) (*elicitation.Response, error) {
			require.NotNil(t, req.Mode)
			require.Equal(t, elicitation.ModeURL, *req.Mode)
			require.NotNil(t, req.URL)
			require.Equal(t, "https://example.com/auth", *req.URL)
			require.NotNil(t, req.ElicitationID)
			require.Equal(t, "elicit-123", *req.ElicitationID)

			return &elicitation.Response{Action: elicitation.ActionCancel}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleMCPElicitation(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":       "mcpServer_elicitation/request",
			"serverName":    "auth-server",
			"message":       "Please authenticate",
			"mode":          "url",
			"url":           "https://example.com/auth",
			"elicitationId": "elicit-123",
			"threadId":      "thread-2",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "cancel", resp["action"])
}

func TestSession_HandleMCPElicitation_NilResponse(t *testing.T) {
	opts := &config.Options{
		OnElicitation: func(_ context.Context, _ *elicitation.Request) (*elicitation.Response, error) {
			return &elicitation.Response{Action: elicitation.ActionDecline}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleMCPElicitation(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype":    "mcpServer_elicitation/request",
			"serverName": "test-server",
			"message":    "test",
			"threadId":   "thread-1",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "decline", resp["action"])
}

func TestSession_HandleCanUseTool_LegacyExecCommandApprovalArray(t *testing.T) {
	var (
		capturedTool  string
		capturedInput map[string]any
	)

	opts := &config.Options{
		CanUseTool: func(_ context.Context, toolName string, input map[string]any, _ *permission.Context) (permission.Result, error) {
			capturedTool = toolName
			capturedInput = input

			return &permission.ResultAllow{}, nil
		},
	}

	session := NewSession(slog.Default(), nil, opts)

	resp, err := session.HandleCanUseTool(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype": "execCommandApproval",
			"command": []any{"rg", "-n", "todo", "."},
			"cwd":     "/tmp/project",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "accept", resp["decision"])
	require.Equal(t, "Bash", capturedTool)
	require.Equal(t, "rg -n todo .", capturedInput["command"])
	require.Equal(t, "/tmp/project", capturedInput["cwd"])
}

func TestSession_HandleChatGPTAuthTokensRefresh_UsesExternalAuthEnv(t *testing.T) {
	t.Setenv("CODEX_CHATGPT_ACCESS_TOKEN", "token-123")
	t.Setenv("CODEX_CHATGPT_ACCOUNT_ID", "acct-123")
	t.Setenv("CODEX_CHATGPT_PLAN_TYPE", "plus")

	session := NewSession(slog.Default(), nil, &config.Options{})

	resp, err := session.HandleChatGPTAuthTokensRefresh(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype": "account_chatgptAuthTokens/refresh",
		},
	})

	require.NoError(t, err)
	require.Equal(t, "token-123", resp["accessToken"])
	require.Equal(t, "acct-123", resp["chatgptAccountId"])
	require.Equal(t, "plus", resp["chatgptPlanType"])
}

func TestSession_HandleChatGPTAuthTokensRefresh_MissingEnvErrors(t *testing.T) {
	session := NewSession(slog.Default(), nil, &config.Options{})

	_, err := session.HandleChatGPTAuthTokensRefresh(context.Background(), &ControlRequest{
		Request: map[string]any{
			"subtype": "account_chatgptAuthTokens/refresh",
		},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "chatgpt auth token refresh requested")
}

func TestConvertMCPContentToItems_ImageContent(t *testing.T) {
	result := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "image", "image_url": "data:image/png;base64,abc"},
			map[string]any{"type": "image", "url": "https://example.com/img.png"},
		},
	}

	items := convertMCPContentToItems(result)

	require.Len(t, items, 3)

	require.Equal(t, "inputText", items[0]["type"])
	require.Equal(t, "hello", items[0]["text"])

	require.Equal(t, "inputImage", items[1]["type"])
	require.Equal(t, "data:image/png;base64,abc", items[1]["imageUrl"])

	require.Equal(t, "inputImage", items[2]["type"])
	require.Equal(t, "https://example.com/img.png", items[2]["imageUrl"])
}
