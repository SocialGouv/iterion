package subprocess

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/stretchr/testify/require"
)

const testThreadID = "thread_test"

// Compile-time check that mockAppServerRPC implements appServerRPC.
var _ appServerRPC = (*mockAppServerRPC)(nil)

// mockAppServerRPC simulates an AppServerTransport for testing the adapter
// without spawning a real codex process.
type mockAppServerRPC struct {
	mu sync.Mutex

	started   bool
	closed    bool
	ready     bool
	readErr   error
	notifyCh  chan *RPCNotification
	requestCh chan *RPCIncomingRequest

	sentRequests    []mockRPCCall
	sendRequestFunc func(ctx context.Context, method string, params any) (*RPCResponse, error)
	sentResponses   []mockRPCResponse
}

type mockRPCCall struct {
	Method string
	Params any
}

type mockRPCResponse struct {
	ID     int64
	Result json.RawMessage
	Error  *RPCError
}

func newMockAppServerRPC() *mockAppServerRPC {
	return &mockAppServerRPC{
		ready:     true,
		notifyCh:  make(chan *RPCNotification, 32),
		requestCh: make(chan *RPCIncomingRequest, 32),
	}
}

func (m *mockAppServerRPC) Start(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.started = true

	return nil
}

func (m *mockAppServerRPC) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.closed {
		m.closed = true
		close(m.notifyCh)
		close(m.requestCh)
	}

	return nil
}

func (m *mockAppServerRPC) IsReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.ready
}

func (m *mockAppServerRPC) ReadErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.readErr
}

func (m *mockAppServerRPC) Notifications() <-chan *RPCNotification {
	return m.notifyCh
}

func (m *mockAppServerRPC) Requests() <-chan *RPCIncomingRequest {
	return m.requestCh
}

func (m *mockAppServerRPC) SendRequest(
	ctx context.Context,
	method string,
	params any,
) (*RPCResponse, error) {
	m.mu.Lock()
	m.sentRequests = append(m.sentRequests, mockRPCCall{Method: method, Params: params})
	fn := m.sendRequestFunc
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, method, params)
	}

	return &RPCResponse{
		JSONRPC: "2.0",
		ID:      1,
		Result:  json.RawMessage(`{}`),
	}, nil
}

func (m *mockAppServerRPC) SendResponse(
	id int64,
	result json.RawMessage,
	rpcErr *RPCError,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sentResponses = append(m.sentResponses, mockRPCResponse{
		ID:     id,
		Result: result,
		Error:  rpcErr,
	})

	return nil
}

func (m *mockAppServerRPC) getSentRequests() []mockRPCCall {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]mockRPCCall, len(m.sentRequests))
	copy(result, m.sentRequests)

	return result
}

func (m *mockAppServerRPC) getSentResponses() []mockRPCResponse {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]mockRPCResponse, len(m.sentResponses))
	copy(result, m.sentResponses)

	return result
}

// newTestAdapter creates an AppServerAdapter with a mock inner transport.
func newTestAdapter(mock *mockAppServerRPC) *AppServerAdapter {
	log := slog.Default()

	adapter := &AppServerAdapter{
		log:                     log.With(slog.String("component", "appserver_adapter")),
		inner:                   mock,
		messages:                make(chan map[string]any, 128),
		errs:                    make(chan error, 4),
		done:                    make(chan struct{}),
		pendingRPCRequests:      make(map[string]int64, 8),
		lastAssistantTextByTurn: make(map[string]string, 8),
		reasoningTextByItem:     make(map[string]*strings.Builder, 8),
		turnHasOutputSchema:     make(map[string]bool, 8),
		sdkMCPServerNames:       make(map[string]struct{}, 8),
	}

	adapter.wg.Add(1)

	go adapter.readLoop()

	return adapter
}

// sendControlRequest is a helper that builds and sends a control_request
// message via the adapter's SendMessage.
func sendControlRequest(
	t *testing.T,
	adapter *AppServerAdapter,
	mock *mockAppServerRPC,
	subtype string,
	extra map[string]any,
) {
	t.Helper()

	request := map[string]any{"subtype": subtype}
	maps.Copy(request, extra)

	msg := map[string]any{
		"type":       "control_request",
		"request_id": "test_req_1",
		"request":    request,
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)

	if mock.sendRequestFunc == nil {
		mock.sendRequestFunc = func(
			_ context.Context,
			_ string,
			_ any,
		) (*RPCResponse, error) {
			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result:  json.RawMessage(`{"thread":{"id":"t1"},"turnId":"turn1"}`),
			}, nil
		}
	}

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)
}

func receiveAdapterMessage(t *testing.T, adapter *AppServerAdapter) map[string]any {
	t.Helper()

	select {
	case msg := <-adapter.messages:
		return msg
	case <-time.After(time.Second):
		t.Fatal("expected adapter message")

		return nil
	}
}

func TestAppServerAdapter_InitializeHandshake(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "initialize", nil)

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)
	require.Equal(t, "thread/start", calls[0].Method)

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "control_response", msg["type"])

		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "success", resp["subtype"])
		require.Equal(t, "test_req_1", resp["request_id"])
	case <-time.After(time.Second):
		t.Fatal("expected control_response message")
	}
}

func TestAppServerAdapter_UserMessage(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.sendRequestFunc = func(
		_ context.Context,
		_ string,
		_ any,
	) (*RPCResponse, error) {
		return &RPCResponse{
			JSONRPC: "2.0",
			ID:      2,
			Result:  json.RawMessage(`{"turnId":"turn_abc"}`),
		}, nil
	}

	userMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "Hello, world!",
		},
	}

	data, err := json.Marshal(userMsg)
	require.NoError(t, err)

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)
	require.Equal(t, "turn/start", calls[0].Method)

	params, ok := calls[0].Params.(map[string]any)
	require.True(t, ok)

	// Input is wrapped as content blocks for the app-server.
	inputBlocks, ok := params["input"].([]map[string]any)
	require.True(t, ok, "input should be []map[string]any")
	require.Len(t, inputBlocks, 1)
	require.Equal(t, "text", inputBlocks[0]["type"])
	require.Equal(t, "Hello, world!", inputBlocks[0]["text"])
}

func TestAppServerAdapter_InterruptRequest(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "interrupt", nil)

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)
	require.Equal(t, "turn/interrupt", calls[0].Method)

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "control_response", msg["type"])
	case <-time.After(time.Second):
		t.Fatal("expected control_response message")
	}
}

func TestAppServerAdapter_InitializeResumeAndFork(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			_ string,
			_ any,
		) (*RPCResponse, error) {
			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result:  json.RawMessage(`{"thread":{"id":"thread_resume"}}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "initialize", map[string]any{
			"resume": "thread_existing",
		})

		calls := mock.getSentRequests()
		require.Len(t, calls, 1)
		require.Equal(t, "thread/resume", calls[0].Method)

		params, ok := calls[0].Params.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "thread_existing", params["threadId"])

		msg := receiveAdapterMessage(t, adapter)
		require.Equal(t, "control_response", msg["type"])
	})

	t.Run("fork", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			_ string,
			_ any,
		) (*RPCResponse, error) {
			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result:  json.RawMessage(`{"thread":{"id":"thread_forked"}}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "initialize", map[string]any{
			"resume":      "thread_existing",
			"forkSession": true,
		})

		calls := mock.getSentRequests()
		require.Len(t, calls, 1)
		require.Equal(t, "thread/fork", calls[0].Method)

		params, ok := calls[0].Params.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "thread_existing", params["threadId"])
	})
}

func TestAppServerAdapter_SetModelAndPermissionOverrides(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "set_model", map[string]any{
		"model": "gpt-5.4",
	})

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_response", msg["type"])

	sendControlRequest(t, adapter, mock, "set_permission_mode", map[string]any{
		"mode": "bypassPermissions",
	})

	msg = receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_response", msg["type"])

	userMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "run tests",
		},
	}

	data, err := json.Marshal(userMsg)
	require.NoError(t, err)

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)
	require.Equal(t, "turn/start", calls[0].Method)

	params, ok := calls[0].Params.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "gpt-5.4", params["model"])
	require.Equal(t, "never", params["approvalPolicy"])

	sandboxPolicy, ok := params["sandboxPolicy"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "dangerFullAccess", sandboxPolicy["type"])
}

func TestAppServerAdapter_ControlRequests_MCPStatusAndUnsupported(t *testing.T) {
	t.Run("mcp_status", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			method string,
			_ any,
		) (*RPCResponse, error) {
			require.Equal(t, "mcpServerStatus/list", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result: json.RawMessage(`{
					"data":[
						{
							"name":"calc",
							"authStatus":"oAuth",
							"tools":{"sum":{"name":"sum","inputSchema":{"type":"object"}}},
							"resources":[{"name":"calc.md","uri":"file:///calc.md"}],
							"resourceTemplates":[{"name":"repo","uriTemplate":"repo://{path}"}]
						},
						{"name":"search","authStatus":"notLoggedIn","tools":{},"resources":[],"resourceTemplates":[]}
					],
					"nextCursor":null
				}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "mcp_status", nil)

		msg := receiveAdapterMessage(t, adapter)
		require.Equal(t, "control_response", msg["type"])

		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "success", resp["subtype"])

		payload, ok := resp["response"].(map[string]any)
		require.True(t, ok)

		servers, ok := payload["mcpServers"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, servers, 2)
		require.Equal(t, "calc", servers[0]["name"])
		require.Equal(t, "connected", servers[0]["status"])
		require.Equal(t, "oAuth", servers[0]["authStatus"])
		require.NotNil(t, servers[0]["tools"])
		require.NotNil(t, servers[0]["resources"])
		require.NotNil(t, servers[0]["resourceTemplates"])
		require.Equal(t, "not_logged_in", servers[1]["status"])
	})

	t.Run("list_models", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)
		callCount := 0

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			method string,
			params any,
		) (*RPCResponse, error) {
			require.Equal(t, "model/list", method)
			require.Equal(t, map[string]any{
				"includeHidden": true,
				"limit":         100,
			}, params)

			callCount++
			require.Equal(t, 1, callCount)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result: json.RawMessage(`{
					"data":[
						{
							"id":"gpt-5.4",
							"model":"gpt-5.4",
							"displayName":"GPT-5.4",
							"description":"Latest GPT-5.4 model",
							"isDefault":true,
							"hidden":false,
							"defaultReasoningEffort":"medium",
							"supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Low"}],
							"inputModalities":["text","image"],
							"supportsPersonality":false,
							"upgrade":"gpt-5.4-pro"
						},
						{
							"id":"o4-mini",
							"model":"o4-mini",
							"displayName":"O4 Mini",
							"isDefault":false,
							"hidden":true
						}
					]
				}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "list_models", nil)

		msg := receiveAdapterMessage(t, adapter)
		require.Equal(t, "control_response", msg["type"])

		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "success", resp["subtype"])

		payload, ok := resp["response"].(map[string]any)
		require.True(t, ok)

		models, ok := payload["models"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, models, 2)
		require.Equal(t, "gpt-5.4", models[0]["id"])
		require.Equal(t, "GPT-5.4", models[0]["displayName"])
		require.Equal(t, true, models[0]["isDefault"])
		require.Equal(t, map[string]any{
			"upgrade":                  "gpt-5.4-pro",
			"modelContextWindow":       1050000,
			"modelContextWindowSource": "official",
			"maxOutputTokens":          128000,
			"maxOutputTokensSource":    "official",
		}, models[0]["metadata"])
		require.Equal(t, "o4-mini", models[1]["id"])
		require.Equal(t, true, models[1]["hidden"])
		_, hasMetadata := models[1]["metadata"]
		require.False(t, hasMetadata)

		_, hasPayloadMetadata := payload["metadata"]
		require.False(t, hasPayloadMetadata)
	})

	t.Run("list_models prefers official metadata and falls back to runtime metadata", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			method string,
			_ any,
		) (*RPCResponse, error) {
			require.Equal(t, "model/list", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result: json.RawMessage(`{
					"data":[
						{"id":"gpt-5.3-codex","model":"gpt-5.3-codex","displayName":"GPT-5.3-Codex"},
						{"id":"gpt-5.3-codex-spark","model":"gpt-5.3-codex-spark","displayName":"GPT-5.3-Codex-Spark"}
					]
				}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "list_models", nil)

		msg := receiveAdapterMessage(t, adapter)
		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)

		payload, ok := resp["response"].(map[string]any)
		require.True(t, ok)

		models, ok := payload["models"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, models, 2)
		require.Equal(t, map[string]any{
			"modelContextWindow":       400000,
			"modelContextWindowSource": "official",
			"maxOutputTokens":          128000,
			"maxOutputTokensSource":    "official",
		}, models[0]["metadata"])
		require.Equal(t, map[string]any{
			"modelContextWindow":       121600,
			"modelContextWindowSource": "runtime",
		}, models[1]["metadata"])
	})

	t.Run("list_models fetches every page internally", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)
		callCount := 0
		errUnexpectedModelListCall := errors.New("unexpected model/list call")

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			method string,
			params any,
		) (*RPCResponse, error) {
			require.Equal(t, "model/list", method)

			callCount++

			switch callCount {
			case 1:
				require.Equal(t, map[string]any{
					"includeHidden": true,
					"limit":         100,
				}, params)

				return &RPCResponse{
					JSONRPC: "2.0",
					ID:      1,
					Result: json.RawMessage(`{
						"data":[{"id":"gpt-5.3-codex","model":"gpt-5.3-codex","displayName":"GPT-5.3-Codex"}],
						"nextCursor":"cursor_123"
					}`),
				}, nil
			case 2:
				require.Equal(t, map[string]any{
					"includeHidden": true,
					"limit":         100,
					"cursor":        "cursor_123",
				}, params)

				return &RPCResponse{
					JSONRPC: "2.0",
					ID:      1,
					Result: json.RawMessage(`{
						"data":[{"id":"gpt-5.4","model":"gpt-5.4","displayName":"GPT-5.4"}]
					}`),
				}, nil
			default:
				t.Fatalf("unexpected model/list call %d", callCount)

				return nil, errUnexpectedModelListCall
			}
		}

		sendControlRequest(t, adapter, mock, "list_models", nil)

		msg := receiveAdapterMessage(t, adapter)
		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)

		payload, ok := resp["response"].(map[string]any)
		require.True(t, ok)

		models, ok := payload["models"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, models, 2)
		require.Equal(t, "gpt-5.3-codex", models[0]["id"])
		require.Equal(t, "gpt-5.4", models[1]["id"])

		_, hasPayloadMetadata := payload["metadata"]
		require.False(t, hasPayloadMetadata)
	})

	t.Run("list_models with empty result", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			method string,
			_ any,
		) (*RPCResponse, error) {
			require.Equal(t, "model/list", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result:  json.RawMessage(`{"data":[]}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "list_models", nil)

		msg := receiveAdapterMessage(t, adapter)
		require.Equal(t, "control_response", msg["type"])

		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "success", resp["subtype"])

		payload, ok := resp["response"].(map[string]any)
		require.True(t, ok)

		models, ok := payload["models"].([]map[string]any)
		require.True(t, ok)
		require.Empty(t, models)
	})

	t.Run("list_models skips entries without id", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.sendRequestFunc = func(
			_ context.Context,
			_ string,
			_ any,
		) (*RPCResponse, error) {
			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result: json.RawMessage(`{
					"data":[
						{"displayName":"No ID model"},
						{"id":"valid","displayName":"Valid"}
					]
				}`),
			}, nil
		}

		sendControlRequest(t, adapter, mock, "list_models", nil)

		msg := receiveAdapterMessage(t, adapter)
		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)

		payload, ok := resp["response"].(map[string]any)
		require.True(t, ok)

		models, ok := payload["models"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, models, 1)
		require.Equal(t, "valid", models[0]["id"])
	})

	t.Run("unsupported subtype returns error response", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		sendControlRequest(t, adapter, mock, "future_subtype", nil)

		msg := receiveAdapterMessage(t, adapter)
		require.Equal(t, "control_response", msg["type"])

		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "error", resp["subtype"])
		require.Contains(t, resp["error"], "unsupported control request")
	})

	t.Run("rewind_files returns unsupported response", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		sendControlRequest(t, adapter, mock, "rewind_files", map[string]any{
			"user_message_id": "msg_123",
		})

		msg := receiveAdapterMessage(t, adapter)
		require.Equal(t, "control_response", msg["type"])

		resp, ok := msg["response"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "error", resp["subtype"])
		require.Contains(t, resp["error"], "rewind_files")
	})
}

func TestAppServerAdapter_NotificationTranslation(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		params         json.RawMessage
		expectedType   string
		checkItem      bool
		expectedFields map[string]any
	}{
		{
			name:         "thread/started",
			method:       "thread/started",
			params:       json.RawMessage(`{}`),
			expectedType: "thread.started",
		},
		{
			name:         "turn/started",
			method:       "turn/started",
			params:       json.RawMessage(`{}`),
			expectedType: "turn.started",
		},
		{
			name:         "turn/completed with usage",
			method:       "turn/completed",
			params:       json.RawMessage(`{"usage":{"input_tokens":100,"output_tokens":50}}`),
			expectedType: "turn.completed",
		},
		{
			name:         "turn/failed",
			method:       "turn/failed",
			params:       json.RawMessage(`{"error":{"message":"something broke"}}`),
			expectedType: "turn.failed",
		},
		{
			name:         "item/started with agentMessage",
			method:       "item/started",
			params:       json.RawMessage(`{"item":{"id":"item1","type":"agentMessage","text":"hello"}}`),
			expectedType: "item.started",
			checkItem:    true,
			expectedFields: map[string]any{
				"type": "agent_message",
				"text": "hello",
			},
		},
		// item/agentMessage/delta is tested separately in
		// TestAppServerAdapter_DeltaSuppression_DisabledByDefault and
		// TestAppServerAdapter_DeltaEmission_WhenEnabled since default
		// behavior suppresses deltas (returns nil).
		{
			name:         "item/completed with commandExecution",
			method:       "item/completed",
			params:       json.RawMessage(`{"item":{"id":"item2","type":"commandExecution","command":"ls"}}`),
			expectedType: "item.completed",
			checkItem:    true,
			expectedFields: map[string]any{
				"type":    "command_execution",
				"command": "ls",
			},
		},
		{
			name:         "item/completed with dynamicToolCall rewrites SDK MCP name",
			method:       "item/completed",
			params:       json.RawMessage(`{"item":{"id":"item3","type":"dynamicToolCall","tool":"sdkmcp__calc__add","arguments":{"a":15,"b":27}}}`),
			expectedType: "item.completed",
			checkItem:    true,
			expectedFields: map[string]any{
				"type": "dynamic_tool_call",
				"tool": "mcp__calc__add",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockAppServerRPC()
			adapter := newTestAdapter(mock)
			adapter.sdkMCPServerNames = map[string]struct{}{
				"calc": {},
			}

			mock.notifyCh <- &RPCNotification{
				JSONRPC: "2.0",
				Method:  tc.method,
				Params:  tc.params,
			}

			select {
			case msg := <-adapter.messages:
				require.Equal(t, tc.expectedType, msg["type"])

				if tc.checkItem {
					item, ok := msg["item"].(map[string]any)
					require.True(t, ok, "expected item field")

					for k, v := range tc.expectedFields {
						require.Equal(t, v, item[k], "field %s mismatch", k)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("expected message from notification")
			}

			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		})
	}
}

func TestBuildInitializeRPC_DynamicToolsPassthrough(t *testing.T) {
	dynamicTools := []map[string]any{
		{
			"name":        "add",
			"description": "Add two numbers",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "number"},
					"b": map[string]any{"type": "number"},
				},
			},
		},
	}

	requestData := map[string]any{
		"subtype":      "initialize",
		"dynamicTools": dynamicTools,
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)

	passedTools, ok := params["dynamicTools"].([]map[string]any)
	require.True(t, ok, "dynamicTools should be passed through")
	require.Len(t, passedTools, 1)
	require.Equal(t, "add", passedTools[0]["name"])
}

func TestBuildInitializeRPC_ConvertsSDKMCPServersToDynamicTools(t *testing.T) {
	requestData := map[string]any{
		"subtype": "initialize",
		"allowedTools": []string{
			"mcp__calc__add",
			"Bash",
		},
		"mcpServers": map[string]any{
			"calc": map[string]any{
				"type": "sdk",
				"tools": []map[string]any{
					{
						"name":        "add",
						"description": "Add two numbers",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"a": map[string]any{"type": "number"},
								"b": map[string]any{"type": "number"},
							},
							"required": []string{"a", "b"},
						},
					},
				},
			},
			"remote": map[string]any{
				"type":    "stdio",
				"command": "remote-mcp",
			},
		},
		"dynamicTools": []map[string]any{
			{
				"name":        "existing_tool",
				"description": "Existing tool",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)

	passedServers, ok := params["mcpServers"].(map[string]any)
	require.True(t, ok, "non-SDK mcpServers should be preserved")
	require.Len(t, passedServers, 1)
	require.Contains(t, passedServers, "remote")
	require.NotContains(t, passedServers, "calc")

	passedTools, ok := params["dynamicTools"].([]map[string]any)
	require.True(t, ok, "SDK MCP tools should be converted to dynamicTools")
	require.Len(t, passedTools, 2)
	require.Equal(t, "existing_tool", passedTools[0]["name"])
	require.Equal(t, "sdkmcp__calc__add", passedTools[1]["name"])
	require.Equal(t, "Add two numbers", passedTools[1]["description"])

	inputSchema, ok := passedTools[1]["inputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", inputSchema["type"])

	allowedTools, ok := params["allowedTools"].([]string)
	require.True(t, ok)
	require.Equal(t, []string{"sdkmcp__calc__add", "Bash"}, allowedTools)
}

func TestBuildInitializeRPC_RejectsSDKMCPDynamicToolNameCollision(t *testing.T) {
	requestData := map[string]any{
		"subtype": "initialize",
		"dynamicTools": []map[string]any{
			{
				"name":        "sdkmcp__calc__add",
				"description": "User-defined tool that collides with generated SDK MCP name",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		"mcpServers": map[string]any{
			"calc": map[string]any{
				"type": "sdk",
				"tools": []map[string]any{
					{
						"name":        "add",
						"description": "Add two numbers",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
				},
			},
		},
	}

	_, _, _, err := buildInitializeRPC(requestData) //nolint:dogsled // testing requires all return values
	require.Error(t, err, "SDK MCP-generated tool names should not be allowed to collide with user dynamic tools")
	require.Contains(t, err.Error(), "sdkmcp__calc__add")
}

func TestAppServerAdapter_Initialize_MergesDynamicToolsAfterJSONRoundTrip(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "initialize", map[string]any{
		"dynamicTools": []map[string]any{
			{
				"name":        "existing_tool",
				"description": "Existing tool",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		"mcpServers": map[string]any{
			"calc": map[string]any{
				"type": "sdk",
				"tools": []map[string]any{
					{
						"name":        "add",
						"description": "Add two numbers",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
				},
			},
		},
	})

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)

	params, ok := calls[0].Params.(map[string]any)
	require.True(t, ok)

	passedTools, ok := params["dynamicTools"].([]map[string]any)
	require.True(t, ok, "dynamicTools should be merged after JSON round-trip")
	require.Len(t, passedTools, 2)
	require.Equal(t, "existing_tool", passedTools[0]["name"])
	require.Equal(t, "sdkmcp__calc__add", passedTools[1]["name"])
}

func TestAppServerAdapter_Initialize_RewritesSDKMCPToolListsAfterJSONRoundTrip(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "initialize", map[string]any{
		"allowedTools":    []string{"mcp__calc__add", "Bash"},
		"disallowedTools": []string{"mcp__calc__subtract"},
		"mcpServers": map[string]any{
			"calc": map[string]any{
				"type": "sdk",
				"tools": []map[string]any{
					{
						"name":        "add",
						"description": "Add two numbers",
					},
					{
						"name":        "subtract",
						"description": "Subtract two numbers",
					},
				},
			},
		},
	})

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)

	params, ok := calls[0].Params.(map[string]any)
	require.True(t, ok)

	allowedTools, ok := params["allowedTools"].([]string)
	require.True(t, ok, "allowedTools should be rewritten after JSON round-trip")
	require.Equal(t, []string{"sdkmcp__calc__add", "Bash"}, allowedTools)

	disallowedTools, ok := params["disallowedTools"].([]string)
	require.True(t, ok, "disallowedTools should be rewritten after JSON round-trip")
	require.Equal(t, []string{"sdkmcp__calc__subtract"}, disallowedTools)
}

func TestAppServerAdapter_Initialize_RewritesSDKMCPToolsListAfterJSONRoundTrip(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "initialize", map[string]any{
		"tools": []string{"mcp__calc__add", "Bash"},
		"mcpServers": map[string]any{
			"calc": map[string]any{
				"type": "sdk",
				"tools": []map[string]any{
					{
						"name":        "add",
						"description": "Add two numbers",
					},
				},
			},
		},
	})

	calls := mock.getSentRequests()
	require.Len(t, calls, 1)

	params, ok := calls[0].Params.(map[string]any)
	require.True(t, ok)

	tools, ok := params["tools"].([]string)
	require.True(t, ok, "tools should be rewritten after JSON round-trip")
	require.Equal(t, []string{"sdkmcp__calc__add", "Bash"}, tools)
}

func TestAppServerAdapter_HandleServerRequest_RewritesSDKMCPToolNames(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.sdkMCPServerNames = map[string]struct{}{
		"calc": {},
	}

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.requestCh <- &RPCIncomingRequest{
		JSONRPC: "2.0",
		ID:      7,
		Method:  "item_tool/call",
		Params:  json.RawMessage(`{"tool":"sdkmcp__calc__add","arguments":{"a":15,"b":27}}`),
	}

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_request", msg["type"])

	req, ok := msg["request"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "item_tool_call", req["subtype"])
	require.Equal(t, "mcp__calc__add", req["tool"])
}

func TestAppServerAdapter_HandleServerRequest_RewritesSDKMCPPermissionToolName(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.sdkMCPServerNames = map[string]struct{}{
		"calc": {},
	}

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.requestCh <- &RPCIncomingRequest{
		JSONRPC: "2.0",
		ID:      8,
		Method:  "can_use_tool",
		Params:  json.RawMessage(`{"tool_name":"sdkmcp__calc__add","input":{"a":15,"b":27}}`),
	}

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_request", msg["type"])

	req, ok := msg["request"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "can_use_tool", req["subtype"])
	require.Equal(t, "mcp__calc__add", req["tool_name"])
}

func TestAppServerAdapter_HandleServerRequest_PreservesPlainDynamicToolUsingSDKMCPPrefix(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.requestCh <- &RPCIncomingRequest{
		JSONRPC: "2.0",
		ID:      9,
		Method:  "item_tool/call",
		Params:  json.RawMessage(`{"tool":"sdkmcp__plain_dynamic_tool","arguments":{"value":"secret"}}`),
	}

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_request", msg["type"])

	req, ok := msg["request"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "item_tool_call", req["subtype"])
	require.Equal(t,
		"sdkmcp__plain_dynamic_tool",
		req["tool"],
		"plain dynamic tools should keep their original name when no SDK MCP server generated this prefix",
	)
}

func TestAppServerAdapter_InitializeFailFastUnsupportedOptions(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	sendControlRequest(t, adapter, mock, "initialize", map[string]any{
		"permissionPromptToolName": "custom",
	})

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_response", msg["type"])

	resp, ok := msg["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "error", resp["subtype"])
	require.Contains(t, resp["error"], "permissionPromptToolName")
}

func TestAppServerAdapter_InitializeTurnOverrides(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	callCount := 0
	mock.sendRequestFunc = func(
		_ context.Context,
		method string,
		params any,
	) (*RPCResponse, error) {
		callCount++
		switch callCount {
		case 1:
			require.Equal(t, "thread/start", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result:  json.RawMessage(`{"thread":{"id":"thread_1"}}`),
			}, nil
		case 2:
			require.Equal(t, "turn/start", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      2,
				Result:  json.RawMessage(`{"turnId":"turn_1"}`),
			}, nil
		default:
			t.Fatalf("unexpected RPC call %d", callCount)

			return &RPCResponse{}, nil
		}
	}

	sendControlRequest(t, adapter, mock, "initialize", map[string]any{
		"reasoningEffort": "high",
		"outputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer": map[string]any{"type": "string"},
			},
			"required": []string{"answer"},
		},
	})

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_response", msg["type"])

	userMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "hello",
		},
	}

	data, err := json.Marshal(userMsg)
	require.NoError(t, err)

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)

	calls := mock.getSentRequests()
	require.Len(t, calls, 2)
	require.Equal(t, "turn/start", calls[1].Method)

	params, ok := calls[1].Params.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "high", params["effort"])

	outputSchema, ok := params["outputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", outputSchema["type"])
}

func TestAppServerAdapter_QueryOutputSchemaPerTurn(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	callCount := 0
	mock.sendRequestFunc = func(
		_ context.Context,
		method string,
		params any,
	) (*RPCResponse, error) {
		callCount++
		switch callCount {
		case 1:
			require.Equal(t, "thread/start", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Result:  json.RawMessage(`{"thread":{"id":"thread_1"}}`),
			}, nil
		case 2:
			require.Equal(t, "turn/start", method)

			return &RPCResponse{
				JSONRPC: "2.0",
				ID:      2,
				Result:  json.RawMessage(`{"turnId":"turn_1"}`),
			}, nil
		default:
			t.Fatalf("unexpected RPC call %d", callCount)

			return &RPCResponse{}, nil
		}
	}

	sendControlRequest(t, adapter, mock, "initialize", map[string]any{})

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "control_response", msg["type"])

	userMsg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "hello",
		},
		"outputSchema": map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
				"required": []string{"answer"},
			},
		},
	}

	data, err := json.Marshal(userMsg)
	require.NoError(t, err)

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)

	calls := mock.getSentRequests()
	require.Len(t, calls, 2)
	require.Equal(t, "turn/start", calls[1].Method)

	params, ok := calls[1].Params.(map[string]any)
	require.True(t, ok)

	outputSchema, ok := params["outputSchema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", outputSchema["type"])
}

func TestAppServerAdapter_TurnCompleted_OverlappingTurnsKeepStructuredOutputPerTurn(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	callCount := 0
	mock.sendRequestFunc = func(
		_ context.Context,
		method string,
		params any,
	) (*RPCResponse, error) {
		callCount++

		require.Equal(t, "turn/start", method)

		return &RPCResponse{
			JSONRPC: "2.0",
			ID:      int64(callCount),
			Result:  json.RawMessage([]byte(`{"turnId":"turn_` + string(rune('0'+callCount)) + `"}`)),
		}, nil
	}

	firstTurn := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "return structured output",
		},
		"outputSchema": map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
				"required": []string{"answer"},
			},
		},
	}

	firstData, err := json.Marshal(firstTurn)
	require.NoError(t, err)
	require.NoError(t, adapter.SendMessage(context.Background(), firstData))

	secondTurn := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "plain text is fine",
		},
	}

	secondData, err := json.Marshal(secondTurn)
	require.NoError(t, err)
	require.NoError(t, adapter.SendMessage(context.Background(), secondData))

	calls := mock.getSentRequests()
	require.Len(t, calls, 2)

	firstParams, ok := calls[0].Params.(map[string]any)
	require.True(t, ok)

	_, firstHasSchema := firstParams["outputSchema"]
	require.True(t, firstHasSchema, "first turn should send an output schema")

	secondParams, ok := calls[1].Params.(map[string]any)
	require.True(t, ok)

	_, secondHasSchema := secondParams["outputSchema"]
	require.False(t, secondHasSchema, "second turn should not send an output schema")

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{"turnId":"turn_1","result":"{\"answer\":\"4\"}"}`),
	}

	msg := receiveAdapterMessage(t, adapter)
	require.Equal(t, "turn.completed", msg["type"])

	structured, ok := msg["structured_output"].(map[string]any)
	require.True(t, ok, "structured output should be keyed to the completing turn, not the most recently started turn")
	require.Equal(t, "4", structured["answer"])
}

func TestAppServerAdapter_TurnCompleted_OverlappingTurnsDoNotReuseOtherTurnFallbackText(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	callCount := 0
	mock.sendRequestFunc = func(
		_ context.Context,
		method string,
		_ any,
	) (*RPCResponse, error) {
		callCount++

		require.Equal(t, "turn/start", method)

		return &RPCResponse{
			JSONRPC: "2.0",
			ID:      int64(callCount),
			Result:  json.RawMessage([]byte(`{"turnId":"turn_` + string(rune('0'+callCount)) + `"}`)),
		}, nil
	}

	firstTurn := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "return structured output",
		},
		"outputSchema": map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
				"required": []string{"answer"},
			},
		},
	}

	firstData, err := json.Marshal(firstTurn)
	require.NoError(t, err)
	require.NoError(t, adapter.SendMessage(context.Background(), firstData))

	secondTurn := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "plain text is fine",
		},
	}

	secondData, err := json.Marshal(secondTurn)
	require.NoError(t, err)
	require.NoError(t, adapter.SendMessage(context.Background(), secondData))

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/completed",
		Params: json.RawMessage(`{
			"item":{
				"id":"item_assistant_turn_2",
				"type":"agentMessage",
				"text":"{\"answer\":\"from-turn-2\"}"
			}
		}`),
	}

	select {
	case <-adapter.messages:
	case <-time.After(time.Second):
		t.Fatal("expected item.completed message")
	}

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{"turnId":"turn_1"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.completed", msg["type"])

		_, hasResult := msg["result"]
		require.False(t,
			hasResult,
			"turn_1 should not inherit fallback text from turn_2 when turn_1 never emitted assistant text",
		)

		_, hasStructured := msg["structured_output"]
		require.False(t,
			hasStructured,
			"turn_1 should not parse structured output from turn_2 fallback text",
		)
	case <-time.After(time.Second):
		t.Fatal("expected turn.completed message")
	}
}

func TestAppServerAdapter_ItemTypeMapping(t *testing.T) {
	tests := []struct {
		camelCase string
		snakeCase string
	}{
		{"agentMessage", "agent_message"},
		{"collabAgentToolCall", "collab_agent_tool_call"},
		{"commandExecution", "command_execution"},
		{"contextCompaction", "context_compaction"},
		{"dynamicToolCall", "dynamic_tool_call"},
		{"enteredReviewMode", "entered_review_mode"},
		{"exitedReviewMode", "exited_review_mode"},
		{"fileChange", "file_change"},
		{"imageView", "image_view"},
		{"mcpToolCall", "mcp_tool_call"},
		{"plan", "plan"},
		{"webSearch", "web_search"},
		{"todoList", "todo_list"},
		{"reasoning", "reasoning"},
		{"error", "error"},
		{"unknownType", "unknownType"},
	}

	for _, tc := range tests {
		t.Run(tc.camelCase, func(t *testing.T) {
			result := camelToSnake(tc.camelCase)
			require.Equal(t, tc.snakeCase, result)
		})
	}
}

func TestAppServerAdapter_ServerRequest_HookCallback(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.requestCh <- &RPCIncomingRequest{
		JSONRPC: "2.0",
		Method:  "hooks/callback",
		ID:      42,
		Params:  json.RawMessage(`{"callback_id":"hook_0","input":{"hook_event_name":"PreToolUse"}}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "control_request", msg["type"])
		require.Equal(t, "rpc_42", msg["request_id"])

		reqData, ok := msg["request"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "hooks_callback", reqData["subtype"])
	case <-time.After(time.Second):
		t.Fatal("expected control_request from server request")
	}

	adapter.mu.Lock()
	rpcID, ok := adapter.pendingRPCRequests["rpc_42"]
	adapter.mu.Unlock()

	require.True(t, ok)
	require.Equal(t, int64(42), rpcID)

	resp := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": "rpc_42",
			"response":   map[string]any{"continue": true},
		},
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)

	responses := mock.getSentResponses()
	require.Len(t, responses, 1)
	require.Equal(t, int64(42), responses[0].ID)
	require.Nil(t, responses[0].Error)
}

func TestAppServerAdapter_Close(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	close(adapter.done)
	mock.Close()
	adapter.wg.Wait()

	_, ok := <-adapter.messages
	require.False(t, ok)
}

func TestAppServerAdapter_EndInput_NoOp(t *testing.T) {
	adapter := &AppServerAdapter{}

	err := adapter.EndInput()
	require.NoError(t, err)
}

func TestAppServerAdapter_ConcurrentSendMessage(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	const numSenders = 10

	var wg sync.WaitGroup

	wg.Add(numSenders)

	for i := range numSenders {
		go func(id int) {
			defer wg.Done()

			userMsg := map[string]any{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": "msg",
				},
			}

			data, err := json.Marshal(userMsg)
			require.NoError(t, err)

			_ = adapter.SendMessage(context.Background(), data)
		}(i)
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent sends deadlocked")
	}
}

func TestAppServerAdapter_IsReady_Delegates(t *testing.T) {
	opts := &config.Options{}
	log := slog.Default()
	adapter := NewAppServerAdapter(log, opts)

	require.False(t, adapter.IsReady())
}

func TestAppServerAdapter_UnknownMessageType(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	unknownMsg := map[string]any{
		"type": "some_unknown_type",
		"data": "test",
	}

	data, err := json.Marshal(unknownMsg)
	require.NoError(t, err)

	err = adapter.SendMessage(context.Background(), data)
	require.NoError(t, err)
}

func TestAppServerAdapter_UserMessageItem(t *testing.T) {
	t.Run("item/started with userMessage emits user message", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.notifyCh <- &RPCNotification{
			JSONRPC: "2.0",
			Method:  "item/started",
			Params: json.RawMessage(`{
				"item":{
					"type":"userMessage",
					"id":"msg_001",
					"content":[{"type":"text","text":"What is 2+2?"}]
				}
			}`),
		}

		select {
		case msg := <-adapter.messages:
			require.Equal(t, "user", msg["type"])

			message, ok := msg["message"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "user", message["role"])
			require.Equal(t, "What is 2+2?", message["content"])
			require.Equal(t, "msg_001", msg["uuid"])
		case <-time.After(time.Second):
			t.Fatal("expected user message from userMessage item/started")
		}
	})

	t.Run("item/started with structured userMessage preserves non-text blocks", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.notifyCh <- &RPCNotification{
			JSONRPC: "2.0",
			Method:  "item/started",
			Params: json.RawMessage(`{
				"item":{
					"type":"userMessage",
					"id":"msg_002",
					"content":[
						{"type":"text","text":"Read this file"},
						{"type":"mention","name":"notes.txt","path":"/tmp/notes.txt"},
						{"type":"image","url":"data:image/png;base64,AQID"}
					]
				}
			}`),
		}

		select {
		case msg := <-adapter.messages:
			require.Equal(t, "user", msg["type"])

			message, ok := msg["message"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "user", message["role"])

			content, ok := message["content"].([]any)
			require.True(t, ok, "structured userMessage content should remain a block array")
			require.Len(t, content, 3, "non-text userMessage blocks should not be dropped")

			mention, ok := content[1].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "mention", mention["type"])
			require.Equal(t, "/tmp/notes.txt", mention["path"])

			image, ok := content[2].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "image", image["type"])
		case <-time.After(time.Second):
			t.Fatal("expected user message from structured userMessage item/started")
		}
	})

	t.Run("item/completed with userMessage emits system message", func(t *testing.T) {
		mock := newMockAppServerRPC()
		adapter := newTestAdapter(mock)

		defer func() {
			close(adapter.done)
			mock.Close()
			adapter.wg.Wait()
		}()

		mock.notifyCh <- &RPCNotification{
			JSONRPC: "2.0",
			Method:  "item/completed",
			Params: json.RawMessage(`{
				"item":{
					"type":"userMessage",
					"id":"msg_001",
					"content":[{"type":"text","text":"What is 2+2?"}]
				}
			}`),
		}

		select {
		case msg := <-adapter.messages:
			require.Equal(t, "system", msg["type"])
			require.Equal(t, "user_message.completed", msg["subtype"])
		case <-time.After(time.Second):
			t.Fatal("expected system message from userMessage item/completed")
		}
	})
}

func TestAppServerAdapter_ReasoningItem_Summary(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/completed",
		Params: json.RawMessage(`{
			"item":{
				"type":"reasoning",
				"id":"reason_1",
				"summary":["The answer is","4."],
				"content":[]
			}
		}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "item.completed", msg["type"])

		item, ok := msg["item"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "reasoning", item["type"])
		require.Equal(t, "The answer is\n4.", item["text"])
	case <-time.After(time.Second):
		t.Fatal("expected item.completed with reasoning text")
	}
}

func TestAppServerAdapter_ReasoningItem_EmptySummary(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/started",
		Params: json.RawMessage(`{
			"item":{
				"type":"reasoning",
				"id":"reason_2",
				"summary":[],
				"content":[]
			}
		}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "item.started", msg["type"])

		item, ok := msg["item"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "reasoning", item["type"])

		// With empty summary, text should not be set.
		_, hasText := item["text"].(string)
		require.False(t, hasText, "empty summary should not produce text field")
	case <-time.After(time.Second):
		t.Fatal("expected item.started for reasoning")
	}
}

func TestAppServerAdapter_TokenUsageCaching(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	// Send token usage first.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "thread/tokenUsage/updated",
		Params: json.RawMessage(`{
			"tokenUsage":{
				"last":{
					"totalTokens":9991,
					"inputTokens":9954,
					"cachedInputTokens":7552,
					"outputTokens":37,
					"reasoningOutputTokens":30
				}
			}
		}`),
	}

	// Drain the system message from tokenUsage.
	select {
	case <-adapter.messages:
	case <-time.After(time.Second):
		t.Fatal("expected system message from token usage")
	}

	// Now send turn/completed without inline usage.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.completed", msg["type"])

		usage, ok := msg["usage"].(map[string]any)
		require.True(t, ok, "expected usage from cached token data")
		require.Equal(t, float64(9954), usage["input_tokens"])
		require.Equal(t, float64(37), usage["output_tokens"])
		require.Equal(t, float64(7552), usage["cached_input_tokens"])
		require.Equal(t, float64(30), usage["reasoning_output_tokens"])
	case <-time.After(time.Second):
		t.Fatal("expected turn.completed with cached usage")
	}
}

func TestAppServerAdapter_TurnCompleted_InlineUsage(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	// Pre-cache some token usage.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "thread/tokenUsage/updated",
		Params: json.RawMessage(`{
			"tokenUsage":{"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}
		}`),
	}

	select {
	case <-adapter.messages:
	case <-time.After(time.Second):
		t.Fatal("expected system message from token usage")
	}

	// Send turn/completed WITH inline usage — should prefer inline.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{"usage":{"input_tokens":500,"output_tokens":200}}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.completed", msg["type"])

		usage, ok := msg["usage"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, float64(500), usage["input_tokens"])
		require.Equal(t, float64(200), usage["output_tokens"])
	case <-time.After(time.Second):
		t.Fatal("expected turn.completed with inline usage")
	}
}

func TestAppServerAdapter_TurnCompleted_ResultFallbackFromAssistantText(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/completed",
		Params: json.RawMessage(`{
			"item":{
				"id":"item_assistant_1",
				"type":"agentMessage",
				"text":"{\"answer\":\"ok\"}"
			}
		}`),
	}

	// Drain the item.completed message.
	select {
	case <-adapter.messages:
	case <-time.After(time.Second):
		t.Fatal("expected item.completed message")
	}

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.completed", msg["type"])
		require.Equal(t, "{\"answer\":\"ok\"}", msg["result"])
	case <-time.After(time.Second):
		t.Fatal("expected turn.completed with result fallback")
	}
}

func TestAppServerAdapter_TurnCompleted_StructuredOutputFallbackFromAssistantText(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.currentTurnHasOutputSchema = true

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/completed",
		Params: json.RawMessage(`{
			"item":{
				"id":"item_assistant_1",
				"type":"agentMessage",
				"text":"{\"answer\":\"4\"}"
			}
		}`),
	}

	select {
	case <-adapter.messages:
	case <-time.After(time.Second):
		t.Fatal("expected item.completed message")
	}

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.completed", msg["type"])
		require.Equal(t, "{\"answer\":\"4\"}", msg["result"])

		structured, ok := msg["structured_output"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "4", structured["answer"])
	case <-time.After(time.Second):
		t.Fatal("expected turn.completed with structured_output fallback")
	}
}

func TestAppServerAdapter_TurnCompleted_StructuredOutputFromResult(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.currentTurnHasOutputSchema = true

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/completed",
		Params:  json.RawMessage(`{"result":"{\"answer\":\"4\"}"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.completed", msg["type"])
		require.Equal(t, "{\"answer\":\"4\"}", msg["result"])

		structured, ok := msg["structured_output"].(map[string]any)
		require.True(t, ok, "expected structured_output alongside JSON result")
		require.Equal(t, "4", structured["answer"])
	case <-time.After(time.Second):
		t.Fatal("expected turn.completed with structured_output parsed from result")
	}
}

func TestAppServerAdapter_TurnFailed_ClearsTurnScopedOutputSchemaState(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	adapter.mu.Lock()
	adapter.turnHasOutputSchema["turn_1"] = true
	adapter.mu.Unlock()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "turn/failed",
		Params:  json.RawMessage(`{"turnId":"turn_1","error":"boom"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "turn.failed", msg["type"])
	case <-time.After(time.Second):
		t.Fatal("expected turn.failed message")
	}

	adapter.mu.Lock()
	_, exists := adapter.turnHasOutputSchema["turn_1"]
	adapter.mu.Unlock()

	require.False(t, exists, "failed turns should not leave turn-scoped output schema state behind")
}

func TestAppServerAdapter_TokenUsageEmitsSystem(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "thread/tokenUsage/updated",
		Params:  json.RawMessage(`{"tokenUsage":{"last":{"totalTokens":42}}}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "system", msg["type"])
		require.Equal(t, "thread.token_usage.updated", msg["subtype"])

		data, ok := msg["data"].(map[string]any)
		require.True(t, ok)
		require.NotNil(t, data["tokenUsage"])
	case <-time.After(time.Second):
		t.Fatal("expected system message from token usage notification")
	}
}

func TestAppServerAdapter_RateLimitsNotification(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "account/rateLimits/updated",
		Params:  json.RawMessage(`{"limits":{"rpm":100}}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "system", msg["type"])
		require.Equal(t, "account.rate_limits.updated", msg["subtype"])

		data, ok := msg["data"].(map[string]any)
		require.True(t, ok)
		require.NotNil(t, data["limits"])
	case <-time.After(time.Second):
		t.Fatal("expected system message from rate limits notification")
	}
}

func TestAppServerAdapter_CodexEvent_Duplicates(t *testing.T) {
	duplicates := []string{
		"codex/event/item_started",
		"codex/event/item_completed",
		"codex/event/agent_message_content_delta",
		"codex/event/agent_message_delta",
		"codex/event/agent_message",
		"codex/event/user_message",
	}

	for _, method := range duplicates {
		t.Run(method, func(t *testing.T) {
			mock := newMockAppServerRPC()
			adapter := newTestAdapter(mock)

			defer func() {
				close(adapter.done)
				mock.Close()
				adapter.wg.Wait()
			}()

			mock.notifyCh <- &RPCNotification{
				JSONRPC: "2.0",
				Method:  method,
				Params:  json.RawMessage(`{"data":"test"}`),
			}

			// Duplicate events should be dropped (nil), so nothing
			// should arrive on the messages channel.
			select {
			case msg := <-adapter.messages:
				t.Fatalf("expected nil for duplicate %s, got: %v", method, msg)
			case <-time.After(100 * time.Millisecond):
				// Expected: no message produced.
			}
		})
	}
}

func TestAppServerAdapter_CodexEvent_Unique(t *testing.T) {
	tests := []struct {
		method          string
		params          string
		expectedSubtype string
		expectedData    map[string]any
	}{
		{
			method:          "codex/event/task_started",
			params:          `{"turnId":"turn_123","collaborationModeKind":"plan","modelContextWindow":256000,"info":"test"}`,
			expectedSubtype: "task.started",
			expectedData: map[string]any{
				"turn_id":                 "turn_123",
				"collaboration_mode_kind": "plan",
				"model_context_window":    float64(256000),
				"info":                    "test",
			},
		},
		{
			method:          "codex/event/task_complete",
			params:          `{"turnId":"turn_123","lastAgentMessage":"done","info":"test"}`,
			expectedSubtype: "task.complete",
			expectedData: map[string]any{
				"turn_id":            "turn_123",
				"last_agent_message": "done",
				"info":               "test",
			},
		},
		{
			method:          "codex/event/thread_rolled_back",
			params:          `{"numTurns":2,"info":"test"}`,
			expectedSubtype: "thread.rolled_back",
			expectedData: map[string]any{
				"num_turns": float64(2),
				"info":      "test",
			},
		},
		{
			method:          "codex/event/token_count",
			params:          `{"info":"test"}`,
			expectedSubtype: "token.count",
			expectedData: map[string]any{
				"info": "test",
			},
		},
		{
			method:          "codex/event/mcp_startup_update",
			params:          `{"info":"test"}`,
			expectedSubtype: "mcp.startup_update",
			expectedData: map[string]any{
				"info": "test",
			},
		},
		{
			method:          "codex/event/mcp_startup_complete",
			params:          `{"info":"test"}`,
			expectedSubtype: "mcp.startup_complete",
			expectedData: map[string]any{
				"info": "test",
			},
		},
		{
			method:          "codex/event/some_future_event",
			params:          `{"info":"test"}`,
			expectedSubtype: "codex.event.some_future_event",
			expectedData: map[string]any{
				"info": "test",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			mock := newMockAppServerRPC()
			adapter := newTestAdapter(mock)

			defer func() {
				close(adapter.done)
				mock.Close()
				adapter.wg.Wait()
			}()

			mock.notifyCh <- &RPCNotification{
				JSONRPC: "2.0",
				Method:  tc.method,
				Params:  json.RawMessage(tc.params),
			}

			select {
			case msg := <-adapter.messages:
				require.Equal(t, "system", msg["type"])
				require.Equal(t, tc.expectedSubtype, msg["subtype"])

				data, ok := msg["data"].(map[string]any)
				require.True(t, ok)
				require.Equal(t, tc.expectedData, data)
			case <-time.After(time.Second):
				t.Fatalf("expected system message for %s", tc.method)
			}
		})
	}
}

func TestAppServerAdapter_DeltaSuppression_DisabledByDefault(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	// includePartialMessages defaults to false in newTestAdapter.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/agentMessage/delta",
		Params:  json.RawMessage(`{"delta":"hello","itemId":"item_1"}`),
	}

	// Delta should be suppressed — nothing should arrive.
	select {
	case msg := <-adapter.messages:
		t.Fatalf("expected delta to be suppressed, got: %v", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: no message produced.
	}
}

func TestAppServerAdapter_DeltaEmission_WhenEnabled(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/agentMessage/delta",
		Params:  json.RawMessage(`{"delta":"world","itemId":"item_2"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "stream_event", msg["type"])
		require.Equal(t, "item_2", msg["uuid"])
		require.Equal(t, testThreadID, msg["session_id"])

		event, ok := msg["event"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "content_block_delta", event["type"])

		delta, ok := event["delta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "text_delta", delta["type"])
		require.Equal(t, "world", delta["text"])
	case <-time.After(time.Second):
		t.Fatal("expected stream_event message from delta")
	}
}

func TestAppServerAdapter_ReasoningTextDeltaEmission_WhenEnabled(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/reasoning/textDelta",
		Params:  json.RawMessage(`{"delta":"thinking","itemId":"reason_1","contentIndex":0,"threadId":"` + testThreadID + `","turnId":"turn_1"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "stream_event", msg["type"],
			"reasoning text deltas should surface as partial stream events instead of generic system messages")
		require.Equal(t, "reason_1", msg["uuid"])
		require.Equal(t, testThreadID, msg["session_id"])

		event, ok := msg["event"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "content_block_delta", event["type"])

		delta, ok := event["delta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "thinking_delta", delta["type"],
			"reasoning deltas must use thinking_delta to distinguish from text_delta")
		require.Equal(t, "thinking", delta["thinking"])
	case <-time.After(time.Second):
		t.Fatal("expected stream_event message from reasoning delta")
	}
}

func TestAppServerAdapter_ReasoningSummaryDeltaEmission(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/reasoning/summaryTextDelta",
		Params:  json.RawMessage(`{"delta":"summary text","itemId":"reason_1","threadId":"` + testThreadID + `"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "stream_event", msg["type"])

		event, ok := msg["event"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "content_block_delta", event["type"])

		delta, ok := event["delta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "thinking_delta", delta["type"],
			"summary deltas must also use thinking_delta type")
		require.Equal(t, "summary text", delta["thinking"])
	case <-time.After(time.Second):
		t.Fatal("expected stream_event from summary delta")
	}
}

func TestAppServerAdapter_ReasoningDeltaAccumulation(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	// Stream reasoning deltas.
	for _, delta := range []string{"Let me ", "think ", "about this."} {
		mock.notifyCh <- &RPCNotification{
			JSONRPC: "2.0",
			Method:  "item/reasoning/textDelta",
			Params:  json.RawMessage(`{"delta":"` + delta + `","itemId":"reason_1","threadId":"` + testThreadID + `"}`),
		}

		// Drain the stream event.
		select {
		case <-adapter.messages:
		case <-time.After(time.Second):
			t.Fatal("expected stream_event from reasoning delta")
		}
	}

	// Complete the reasoning item with an empty summary.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/completed",
		Params: json.RawMessage(`{
			"item":{
				"type":"reasoning",
				"id":"reason_1",
				"summary":[],
				"content":[]
			}
		}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "item.completed", msg["type"])

		item, ok := msg["item"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "reasoning", item["type"])
		require.Equal(t, "Let me think about this.", item["text"],
			"accumulated reasoning deltas should populate text when summary is empty")
	case <-time.After(time.Second):
		t.Fatal("expected item.completed with accumulated reasoning text")
	}

	// Verify accumulator was cleaned up.
	adapter.mu.Lock()
	_, exists := adapter.reasoningTextByItem["reason_1"]
	adapter.mu.Unlock()

	require.False(t, exists, "accumulator entry should be cleaned up after item.completed")
}

func TestAppServerAdapter_ReasoningDeltaAccumulation_SummaryTakesPrecedence(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	// Stream a reasoning delta.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/reasoning/textDelta",
		Params:  json.RawMessage(`{"delta":"raw reasoning","itemId":"reason_2","threadId":"` + testThreadID + `"}`),
	}

	select {
	case <-adapter.messages:
	case <-time.After(time.Second):
		t.Fatal("expected stream_event")
	}

	// Complete with a populated summary — summary should win.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/completed",
		Params: json.RawMessage(`{
			"item":{
				"type":"reasoning",
				"id":"reason_2",
				"summary":["The answer is","42."],
				"content":[]
			}
		}`),
	}

	select {
	case msg := <-adapter.messages:
		item, ok := msg["item"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "The answer is\n42.", item["text"],
			"populated summary should take precedence over accumulated deltas")
	case <-time.After(time.Second):
		t.Fatal("expected item.completed")
	}
}

func TestAppServerAdapter_CommandOutputDeltaEmission(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/commandExecution/outputDelta",
		Params:  json.RawMessage(`{"delta":"< HTTP/2.0 200 OK\n","itemId":"call_42","threadId":"` + testThreadID + `"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "stream_event", msg["type"])
		require.Equal(t, "call_42", msg["uuid"])
		require.Equal(t, testThreadID, msg["session_id"])

		event, ok := msg["event"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "content_block_delta", event["type"])

		delta, ok := event["delta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "command_output_delta", delta["type"],
			"shell stdout/stderr deltas must use command_output_delta to distinguish from assistant prose")
		require.Equal(t, "< HTTP/2.0 200 OK\n", delta["text"])
		require.Equal(t, "call_42", delta["item_id"],
			"command_output_delta must carry item_id so consumers can correlate with the ToolUseBlock")
	case <-time.After(time.Second):
		t.Fatal("expected stream_event message from command output delta")
	}
}

func TestAppServerAdapter_FileChangeDeltaEmission(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/fileChange/outputDelta",
		Params:  json.RawMessage(`{"delta":"+ added line\n","itemId":"call_diff","threadId":"` + testThreadID + `"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "stream_event", msg["type"])
		require.Equal(t, "call_diff", msg["uuid"])

		event, ok := msg["event"].(map[string]any)
		require.True(t, ok)

		delta, ok := event["delta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "file_change_delta", delta["type"],
			"file change diff deltas must use file_change_delta to distinguish from assistant prose")
		require.Equal(t, "+ added line\n", delta["text"])
		require.Equal(t, "call_diff", delta["item_id"])
	case <-time.After(time.Second):
		t.Fatal("expected stream_event message from file change delta")
	}
}

func TestAppServerAdapter_OutputDeltas_SuppressedWhenDisabled(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	// includePartialMessages defaults to false in newTestAdapter, so neither
	// command output nor file change deltas should reach consumers.
	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/commandExecution/outputDelta",
		Params:  json.RawMessage(`{"delta":"output","itemId":"call_x"}`),
	}

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/fileChange/outputDelta",
		Params:  json.RawMessage(`{"delta":"diff","itemId":"call_y"}`),
	}

	select {
	case msg := <-adapter.messages:
		t.Fatalf("expected output deltas to be suppressed, got: %v", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: both deltas suppressed.
	}
}

func TestAppServerAdapter_PlanDeltaEmission(t *testing.T) {
	// item/plan/delta intentionally still uses text_delta. Plans are prose,
	// so consumers can render them as assistant text without ambiguity.
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)
	adapter.includePartialMessages = true
	adapter.threadID = testThreadID

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "item/plan/delta",
		Params:  json.RawMessage(`{"delta":"step 1","itemId":"plan_1","threadId":"` + testThreadID + `"}`),
	}

	select {
	case msg := <-adapter.messages:
		event, ok := msg["event"].(map[string]any)
		require.True(t, ok)

		delta, ok := event["delta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "text_delta", delta["type"])
		require.Equal(t, "step 1", delta["text"])
	case <-time.After(time.Second):
		t.Fatal("expected stream_event from plan delta")
	}
}

func TestAppServerAdapter_UnknownNotification_PassThrough(t *testing.T) {
	mock := newMockAppServerRPC()
	adapter := newTestAdapter(mock)

	defer func() {
		close(adapter.done)
		mock.Close()
		adapter.wg.Wait()
	}()

	mock.notifyCh <- &RPCNotification{
		JSONRPC: "2.0",
		Method:  "some/future/method",
		Params:  json.RawMessage(`{"key":"value"}`),
	}

	select {
	case msg := <-adapter.messages:
		require.Equal(t, "system", msg["type"])
		require.Equal(t, "some/future/method", msg["subtype"])

		data, ok := msg["data"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "value", data["key"])
	case <-time.After(time.Second):
		t.Fatal("expected pass-through system message for unknown notification")
	}
}

func TestBuildInitializeRPC_Personality(t *testing.T) {
	requestData := map[string]any{
		"subtype":     "initialize",
		"personality": "pragmatic",
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)
	require.Equal(t, "pragmatic", params["personality"])
}

func TestBuildInitializeRPC_ServiceTier(t *testing.T) {
	requestData := map[string]any{
		"subtype":     "initialize",
		"serviceTier": "fast",
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)
	require.Equal(t, "fast", params["serviceTier"])
}

func TestBuildInitializeRPC_DeveloperInstructions(t *testing.T) {
	requestData := map[string]any{
		"subtype":               "initialize",
		"developerInstructions": "Always respond in JSON",
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)
	require.Equal(t, "Always respond in JSON", params["developerInstructions"])
}

func TestBuildInitializeRPC_DeveloperInstructionsPrecedence(t *testing.T) {
	requestData := map[string]any{
		"subtype":               "initialize",
		"systemPrompt":          "from system prompt",
		"developerInstructions": "from dev instructions",
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)
	require.Equal(t, "from dev instructions", params["developerInstructions"],
		"explicit developerInstructions should take precedence over systemPrompt")
}

func TestBuildInitializeRPC_SystemPromptFallback(t *testing.T) {
	requestData := map[string]any{
		"subtype":      "initialize",
		"systemPrompt": "from system prompt",
	}

	method, params, _, err := buildInitializeRPC(requestData)
	require.NoError(t, err)
	require.Equal(t, "thread/start", method)
	require.Equal(t, "from system prompt", params["developerInstructions"],
		"systemPrompt should still map to developerInstructions when no explicit DeveloperInstructions")
}

func TestCamelToSnake_NewItemTypes(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"toolSearchCall", "tool_search_call"},
		{"imageGenerationCall", "image_generation_call"},
		{"customToolCall", "custom_tool_call"},
		{"agentMessage", "agent_message"},
		{"unknownType", "unknownType"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			require.Equal(t, tt.expected, camelToSnake(tt.input))
		})
	}
}

func TestTranslateErrorNotification(t *testing.T) {
	adapter := &AppServerAdapter{
		log: slog.Default(),
	}

	t.Run("nested error object", func(t *testing.T) {
		result := adapter.translateErrorNotification(map[string]any{
			"error": map[string]any{
				"message": "web_search is incompatible with minimal effort",
			},
			"threadId": "thread_1",
		})

		require.Equal(t, "error", result["type"])
		require.Equal(t, "web_search is incompatible with minimal effort", result["message"])
	})

	t.Run("top-level message field", func(t *testing.T) {
		result := adapter.translateErrorNotification(map[string]any{
			"message": "something went wrong",
		})

		require.Equal(t, "error", result["type"])
		require.Equal(t, "something went wrong", result["message"])
	})

	t.Run("empty error falls back to unknown", func(t *testing.T) {
		result := adapter.translateErrorNotification(map[string]any{})

		require.Equal(t, "error", result["type"])
		require.Equal(t, "unknown error", result["message"])
	})
}

func TestTranslateNotification_ErrorMethod(t *testing.T) {
	adapter := &AppServerAdapter{
		log:      slog.Default(),
		messages: make(chan map[string]any, 10),
		done:     make(chan struct{}),
	}

	notif := &RPCNotification{
		JSONRPC: "2.0",
		Method:  "error",
		Params:  json.RawMessage(`{"error":{"message":"test error from CLI"},"threadId":"t1"}`),
	}

	event := adapter.translateNotification(notif)

	require.NotNil(t, event)
	require.Equal(t, "error", event["type"])
	require.Equal(t, "test error from CLI", event["message"])
}

func TestEnsureDefaultModel(t *testing.T) {
	t.Parallel()

	t.Run("no default set picks gpt-5.5", func(t *testing.T) {
		t.Parallel()

		models := []map[string]any{
			{"id": "gpt-4.1-mini"},
			{"id": "gpt-5.5"},
			{"id": "o3-pro"},
		}

		ensureDefaultModel(models)

		require.Equal(t, true, models[1]["isDefault"])
		require.Nil(t, models[0]["isDefault"])
		require.Nil(t, models[2]["isDefault"])
	})

	t.Run("existing default is preserved", func(t *testing.T) {
		t.Parallel()

		models := []map[string]any{
			{"id": "gpt-4.1-mini", "isDefault": true},
			{"id": "gpt-5.3-codex"},
		}

		ensureDefaultModel(models)

		require.Equal(t, true, models[0]["isDefault"])
		require.Nil(t, models[1]["isDefault"])
	})

	t.Run("fallback model not present is noop", func(t *testing.T) {
		t.Parallel()

		models := []map[string]any{
			{"id": "gpt-4.1-mini"},
			{"id": "o3-pro"},
		}

		ensureDefaultModel(models)

		require.Nil(t, models[0]["isDefault"])
		require.Nil(t, models[1]["isDefault"])
	})
}
