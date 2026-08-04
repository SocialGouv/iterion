package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/operatormcp"
)

// driveOperator feeds JSON-RPC requests to runOperatorMCPServer and
// returns the decoded responses in order (the mcp_board_test drive
// pattern).
func driveOperator(t *testing.T, srv *operatormcp.Server, lines []string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	out := &bytes.Buffer{}
	if err := runOperatorMCPServer(in, out, srv); err != nil && err != io.EOF {
		t.Fatalf("runOperatorMCPServer: %v", err)
	}
	dec := json.NewDecoder(out)
	var resps []map[string]any
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		resps = append(resps, m)
	}
	return resps
}

func newOperatorTestServer(t *testing.T) *operatormcp.Server {
	t.Helper()
	return &operatormcp.Server{StoreDir: t.TempDir(), WorkDir: t.TempDir()}
}

func TestMCPOperator_Initialize(t *testing.T) {
	resps := driveOperator(t, newOperatorTestServer(t), []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
	})
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	result, _ := resps[0]["result"].(map[string]any)
	srv, _ := result["serverInfo"].(map[string]any)
	if srv["name"] != "iterion" {
		t.Fatalf("serverInfo.name=%v", srv["name"])
	}
}

func TestMCPOperator_ToolsListCarriesAnnotations(t *testing.T) {
	resps := driveOperator(t, newOperatorTestServer(t), []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	})
	tools := resps[0]["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("no tools listed")
	}
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		byName[tool["name"].(string)] = tool
	}
	runTool, ok := byName["local_run"]
	if !ok {
		t.Fatalf("local_run missing from %d tools", len(tools))
	}
	if runTool["annotations"].(map[string]any)["readOnlyHint"] != false {
		t.Fatalf("local_run should carry readOnlyHint=false: %v", runTool["annotations"])
	}
	listTool := byName["local_runs_list"]
	if listTool["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("local_runs_list should carry readOnlyHint=true: %v", listTool["annotations"])
	}
}

func TestMCPOperator_ToolsCallRoundTrip(t *testing.T) {
	srv := newOperatorTestServer(t)
	resps := driveOperator(t, srv, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"local_board_create_issue","arguments":{"title":"wired"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"local_runs_list","arguments":{}}}`,
	})
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d", len(resps))
	}
	created := resps[0]["result"].(map[string]any)
	if created["isError"] == true {
		t.Fatalf("create_issue failed: %+v", created)
	}
	text := created["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "wired") {
		t.Fatalf("issue title missing: %s", text)
	}
	runs := resps[1]["result"].(map[string]any)
	if runs["isError"] == true {
		t.Fatalf("runs_list failed: %+v", runs)
	}
}

func TestMCPOperator_UnknownToolIsMethodLevelError(t *testing.T) {
	resps := driveOperator(t, newOperatorTestServer(t), []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	})
	errObj, ok := resps[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("want a JSON-RPC error, got %+v", resps[0])
	}
	if int(errObj["code"].(float64)) != -32601 {
		t.Fatalf("want -32601, got %v", errObj["code"])
	}
}

func TestMCPOperator_MethodNotFound(t *testing.T) {
	resps := driveOperator(t, newOperatorTestServer(t), []string{
		`{"jsonrpc":"2.0","id":1,"method":"random/method"}`,
	})
	if int(resps[0]["error"].(map[string]any)["code"].(float64)) != -32601 {
		t.Fatal("expected -32601")
	}
}
