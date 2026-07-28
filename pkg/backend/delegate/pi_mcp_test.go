package delegate

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeMCPServer speaks just enough MCP JSON-RPC to stand in for iterion's
// board endpoint: initialize, tools/list, tools/call.
type fakeMCPServer struct {
	mu sync.Mutex
	// toolName is what tools/list advertises. It defaults to the BARE name a
	// real server returns (iterion's board answers `create_issue`); the
	// `mcp__server__tool` prefix is a client-side convention.
	toolName  string
	calls     []string
	calledAs  []string
	gotAuth   string
	callCount int
}

func (f *fakeMCPServer) advertised() string {
	if f.toolName == "" {
		return "create_issue"
	}
	return f.toolName
}

func (f *fakeMCPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		f.mu.Lock()
		f.calls = append(f.calls, req.Method)
		if v := r.Header.Get("X-Iterion-Run"); v != "" {
			f.gotAuth = v
		}
		f.mu.Unlock()

		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "fake-board", "version": "1.0.0"},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{{
				"name":        f.advertised(),
				"description": "Create a card",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"title": map[string]any{"type": "string"}},
				},
			}}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			f.mu.Lock()
			f.calledAs = append(f.calledAs, p.Name)
			f.callCount++
			f.mu.Unlock()
			resp["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "created " + argString(p.Arguments, "title")}},
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// argString reads a string argument, or "?" when it is absent — so a wrong
// argument shape shows up in the assertion rather than as a panic.
func argString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return "?"
}

func (f *fakeMCPServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMCPServer) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotAuth
}

// toolCallNames returns the tool names the server was actually CALLED with,
// which must be its own bare names — the `mcp__server__tool` form the agent
// sees is a client-side registration detail and is never sent on the wire.
func (f *fakeMCPServer) toolCallNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calledAs...)
}

// The board is offered only when the run can actually use it. Registering it
// otherwise hands the agent tools that fail on every call, which costs turns.
func TestPiMCPServers(t *testing.T) {
	full := Task{
		Capabilities:      []string{"board.create"},
		BoardHTTPEndpoint: "http://host/api/v1/mcp/board",
		BoardRunToken:     "tok-123",
	}
	t.Run("board offered when capabilities and endpoint are wired", func(t *testing.T) {
		servers := piMCPServers(full, nil)
		if len(servers) != 1 || servers[0].Name != "iterion_board" {
			t.Fatalf("servers = %+v, want the board", servers)
		}
		if servers[0].Headers["X-Iterion-Run"] != "tok-123" {
			t.Errorf("run token missing from headers: %+v", servers[0].Headers)
		}
	})

	for name, task := range map[string]Task{
		"no capabilities": {BoardHTTPEndpoint: "http://h", BoardRunToken: "t"},
		// Sandboxed without the HTTP pair there is no reachable transport:
		// the stdio server would run on the host, outside the container.
		"sandboxed with no endpoint": {Capabilities: []string{"board.create"}, Sandbox: &recordingRun{}, BoardRunToken: "t"},
		"sandboxed with no token":    {Capabilities: []string{"board.create"}, Sandbox: &recordingRun{}, BoardHTTPEndpoint: "http://h"},
	} {
		t.Run("board withheld: "+name, func(t *testing.T) {
			if got := piMCPServers(task, nil); len(got) != 0 {
				t.Errorf("servers = %+v, want none — the tools would fail on every call", got)
			}
		})
	}

	// The runtime only fills BoardHTTPEndpoint/BoardRunToken for SANDBOXED
	// runs, so keying the board on them alone left every non-sandboxed pi node
	// (sandbox: none, cloud runner pods, any host with no container runtime)
	// with no board at all — silently, unlike claude_code which wires the
	// stdio server exactly for this case.
	t.Run("board rides stdio when the run is not sandboxed", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "iterion")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ITERION_BIN", bin)

		servers := piMCPServers(Task{
			Capabilities: []string{"board.create", "board.move"},
			StoreDir:     "/store",
		}, nil)
		if len(servers) != 1 || servers[0].Name != boardMCPServerName {
			t.Fatalf("servers = %+v, want the board over stdio", servers)
		}
		if servers[0].Transport != "stdio" || !slices.Contains(servers[0].Args, boardMCPSubcommand) {
			t.Errorf("server = %+v, want the %s subcommand over stdio", servers[0], boardMCPSubcommand)
		}
		if servers[0].Env["ITERION_BOARD_CAPS"] != "board.create,board.move" {
			t.Errorf("granted caps not forwarded: %+v", servers[0].Env)
		}
		if servers[0].Env["ITERION_STORE_DIR"] != "/store" {
			t.Errorf("store dir not forwarded: %+v", servers[0].Env)
		}
	})

	// The token must ride inside the server descriptor, never as its own
	// variable, so a generic environment dump cannot log it.
	t.Run("token appears only inside the server descriptor", func(t *testing.T) {
		env := piExtensionEnv(full, nil)
		for k, v := range env {
			if k != "ITERION_PI_MCP_SERVERS" && v == "tok-123" {
				t.Errorf("run token leaked into %s", k)
			}
		}
		if !strings.Contains(env["ITERION_PI_MCP_SERVERS"], "tok-123") {
			t.Error("run token missing from the server descriptor")
		}
	})
}

// Workflow-declared servers reach pi on every transport it supports. A server
// missing what its transport needs is dropped rather than forwarded: the tools
// would fail on every call, and the model would burn turns finding that out.
func TestPiMCPServersForwardsDeclaredServers(t *testing.T) {
	task := Task{
		MCPServers: []TaskMCPServer{
			{Name: "docs", Transport: "http", URL: "https://docs.example/mcp", Headers: map[string]string{"authorization": "Bearer x"}},
			{Name: "legacy", Transport: "sse", URL: "https://legacy.example/sse"},
			{Name: "local", Transport: "stdio", Command: "npx", Args: []string{"-y", "srv"}, Env: map[string]string{"TOKEN": "t"}},
			{Name: "implicit", Command: "./srv"},
			{Name: "broken-http", Transport: "http"},
			{Name: "broken-stdio", Transport: "stdio"},
		},
	}

	got := piMCPServers(task, nil)
	byName := map[string]piMCPServerSpec{}
	for _, s := range got {
		byName[s.Name] = s
	}

	if len(got) != 4 {
		t.Fatalf("forwarded %d servers (%+v), want the 4 usable ones", len(got), got)
	}
	for _, name := range []string{"broken-http", "broken-stdio"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%s was forwarded; it cannot connect and its tools would fail on every call", name)
		}
	}
	if s := byName["docs"]; s.Transport != "http" || s.URL == "" || s.Headers["authorization"] == "" {
		t.Errorf("http server mangled: %+v", s)
	}
	if s := byName["legacy"]; s.Transport != "sse" || s.URL == "" {
		t.Errorf("sse server mangled: %+v", s)
	}
	if s := byName["local"]; s.Transport != "stdio" || s.Command != "npx" || len(s.Args) != 2 || s.Env["TOKEN"] != "t" {
		t.Errorf("stdio server mangled: %+v", s)
	}
	// An empty transport means stdio, matching the DSL's own default.
	if s := byName["implicit"]; s.Transport != "stdio" || s.Command != "./srv" {
		t.Errorf("implicit transport should be stdio: %+v", s)
	}
}

// The board and workflow-declared servers coexist: granting capabilities must
// not cost a node the MCP servers its workflow asked for, nor the reverse.
func TestPiMCPServersBoardAndDeclaredCoexist(t *testing.T) {
	task := Task{
		Capabilities:      []string{"board.create"},
		BoardHTTPEndpoint: "http://host/api/v1/mcp/board",
		BoardRunToken:     "tok",
		MCPServers:        []TaskMCPServer{{Name: "docs", Transport: "http", URL: "https://docs.example/mcp"}},
	}
	got := piMCPServers(task, nil)
	if len(got) != 2 || got[0].Name != "iterion_board" || got[1].Name != "docs" {
		t.Fatalf("servers = %+v, want the board then the declared server", got)
	}
}

// TestPiRPCLiveMCPBridge is the end-to-end proof: a real pi session, the real
// extension, a real HTTP MCP server — and the model successfully calling a
// tool that pi itself has no client for.
func TestPiRPCLiveMCPBridge(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	fake := &fakeMCPServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
	// The server advertises the bare `create_issue`; the agent must see it
	// under iterion's reserved namespace, which is what the permission layer
	// exempts as infrastructure. Driving the mock with the FQN is therefore
	// also the assertion that the prefix was applied at registration.
	t.Setenv("ITERION_PI_MOCK_TOOL", "mcp__iterion_board__create_issue")
	t.Setenv("ITERION_PI_MOCK_TOOL_ARGS", `{"title":"a new card"}`)
	t.Setenv("ITERION_PI_MOCK_TEXT", "card created")

	dir := t.TempDir()
	var toolOut []string
	var mu sync.Mutex
	task := Task{
		NodeID: "board", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "create a card", Model: "mock/scripted",
		Capabilities:      []string{"board.create"},
		BoardHTTPEndpoint: srv.URL,
		BoardRunToken:     "run-token-xyz",
		Hooks: TaskHooks{OnToolCalled: func(name, id string, isErr bool, out string) {
			mu.Lock()
			toolOut = append(toolOut, name+"|"+out)
			mu.Unlock()
		}},
	}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	if _, err := rpc.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	methods := fake.methods()
	if !slices.Contains(methods, "initialize") || !slices.Contains(methods, "tools/list") {
		t.Fatalf("MCP handshake never happened; server saw %v", methods)
	}
	if !slices.Contains(methods, "tools/call") {
		t.Fatalf("the bridged tool was never invoked; server saw %v", methods)
	}
	if fake.auth() != "run-token-xyz" {
		t.Errorf("X-Iterion-Run = %q, want the run token — the server would reject this", fake.auth())
	}
	// The prefix must not leak onto the wire: the server knows only its own
	// bare names and would answer -32601 for a qualified one.
	if got := fake.toolCallNames(); !slices.Contains(got, "create_issue") {
		t.Errorf("tools/call used %v, want the server's bare name create_issue", got)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(toolOut, " ")
	if !strings.Contains(joined, "created a new card") {
		t.Errorf("the board tool's result never reached the model: %q", joined)
	}
}

// TestPiRPCLiveMCPStdioBridge proves the stdio transport end to end: a real pi
// session, the real extension, and a real child-process MCP server that pi has
// no client for. stdio is the transport most `mcp_server:` blocks declare, so
// this is what decides whether such a workflow can run on pi at all.
func TestPiRPCLiveMCPStdioBridge(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH: %v", err)
	}
	server, err := filepath.Abs(filepath.Join("testdata", "mock-mcp-stdio.mjs"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "mcp-server.log")

	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
	t.Setenv("ITERION_PI_MOCK_TOOL", "mcp__probe__echo")
	t.Setenv("ITERION_PI_MOCK_TOOL_ARGS", `{"word":"pomme"}`)
	t.Setenv("ITERION_PI_MOCK_TEXT", "done")

	var toolOut []string
	var mu sync.Mutex
	task := Task{
		NodeID: "stdio", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "echo something", Model: "mock/scripted",
		MCPServers: []TaskMCPServer{{
			Name:      "probe",
			Transport: "stdio",
			Command:   node,
			Args:      []string{server},
			Env: map[string]string{
				"ITERION_TEST_MCP_LOG":      logPath,
				"ITERION_TEST_MCP_GREETING": "bonjour",
			},
		}},
		Hooks: TaskHooks{OnToolCalled: func(name, id string, isErr bool, out string) {
			mu.Lock()
			toolOut = append(toolOut, name+"|"+out)
			mu.Unlock()
		}},
	}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	if _, err := rpc.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the stdio MCP server never ran: %v", err)
	}
	log := string(raw)
	for _, want := range []string{"method:initialize", "method:tools/list", "method:tools/call"} {
		if !strings.Contains(log, want) {
			t.Errorf("server never saw %s; log:\n%s", want, log)
		}
	}
	// The declared env must reach the child, or a server needing a token
	// would start and then fail every call.
	if !strings.Contains(log, "env:bonjour") {
		t.Errorf("declared env never reached the server; log:\n%s", log)
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(toolOut, " ")
	if !strings.Contains(joined, "stdio echoed pomme") {
		t.Errorf("the stdio tool's result never reached the model: %q", joined)
	}
}

// sseMCPServer is a legacy HTTP+SSE MCP server (the 2024-11-05 binding): a
// long-lived GET carries every response, and the POST endpoint is announced on
// that stream rather than configured.
type sseMCPServer struct {
	mu      sync.Mutex
	calls   []string
	out     chan string
	started chan struct{}
	once    sync.Once
}

func newSSEMCPServer() *sseMCPServer {
	return &sseMCPServer{out: make(chan string, 8), started: make(chan struct{})}
}

func (s *sseMCPServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// The endpoint announcement is the first thing on the stream; a client
		// cannot send anything before it arrives.
		_, _ = w.Write([]byte("event: endpoint\ndata: /messages\n\n"))
		flusher.Flush()
		s.once.Do(func() { close(s.started) })

		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-s.out:
				_, _ = w.Write([]byte("event: message\ndata: " + msg + "\n\n"))
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		s.mu.Lock()
		s.calls = append(s.calls, req.Method)
		s.mu.Unlock()

		// A notification takes no answer, only an acknowledgement.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "fake-sse", "version": "1.0.0"},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{{
				"name":        "mcp__legacy__ping",
				"description": "Ping over the legacy transport",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			}}}
		case "tools/call":
			resp["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "sse pong"}},
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
		}
		raw, _ := json.Marshal(resp)
		w.WriteHeader(http.StatusAccepted)
		s.out <- string(raw)
	})
	return mux
}

func (s *sseMCPServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// TestPiRPCLiveMCPSSEBridge proves the legacy SSE transport end to end. Its
// shape is the one most likely to be got wrong — responses arrive on a stream
// opened before the request existed, and the POST URL is discovered, not
// configured — so a real server is the only honest check.
func TestPiRPCLiveMCPSSEBridge(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	fake := newSSEMCPServer()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
	t.Setenv("ITERION_PI_MOCK_TOOL", "mcp__legacy__ping")
	t.Setenv("ITERION_PI_MOCK_TOOL_ARGS", `{}`)
	t.Setenv("ITERION_PI_MOCK_TEXT", "done")

	dir := t.TempDir()
	var toolOut []string
	var mu sync.Mutex
	task := Task{
		NodeID: "sse", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "ping", Model: "mock/scripted",
		MCPServers: []TaskMCPServer{{Name: "legacy", Transport: "sse", URL: srv.URL + "/sse"}},
		Hooks: TaskHooks{OnToolCalled: func(name, id string, isErr bool, out string) {
			mu.Lock()
			toolOut = append(toolOut, name+"|"+out)
			mu.Unlock()
		}},
	}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	if _, err := rpc.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	methods := fake.methods()
	for _, want := range []string{"initialize", "tools/list", "tools/call"} {
		if !slices.Contains(methods, want) {
			t.Errorf("server never saw %s; saw %v", want, methods)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if joined := strings.Join(toolOut, " "); !strings.Contains(joined, "sse pong") {
		t.Errorf("the sse tool's result never reached the model: %q", joined)
	}
}

// A server that accepts the connection and then goes quiet must cost its own
// tools, never the run: the bridge happens inside pi's session_start, which
// iterion's RPC handshake is itself waiting on. Left unbounded, one such server
// takes down every pi run that declares it.
func TestPiRPCLiveMCPHangingServerDoesNotKillTheRun(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	// A bare listener that accepts and never answers. Deliberately not an
	// httptest server: its Close waits for the handler, and the handler here
	// is by definition the thing that never returns.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	blackhole := "http://" + ln.Addr().String()

	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
	t.Setenv("ITERION_PI_MCP_CONNECT_TIMEOUT_MS", "1500")
	t.Setenv("ITERION_PI_MOCK_TEXT", "still alive")

	dir := t.TempDir()
	var said []string
	var mu sync.Mutex
	task := Task{
		NodeID: "hang", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "say something", Model: "mock/scripted",
		MCPServers: []TaskMCPServer{{Name: "blackhole", Transport: "http", URL: blackhole}},
		Hooks: TaskHooks{OnAssistantText: func(text string) {
			mu.Lock()
			said = append(said, text)
			mu.Unlock()
		}},
	}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	if _, err := rpc.Execute(context.Background(), task); err != nil {
		t.Fatalf("a hanging MCP server took the run down: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if joined := strings.Join(said, " "); !strings.Contains(joined, "still alive") {
		t.Errorf("assistant text = %q, want the model's answer despite the dead server", joined)
	}
}
