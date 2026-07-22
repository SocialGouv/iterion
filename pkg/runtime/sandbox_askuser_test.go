package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/askusermcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestWorkflowHasInteractiveNode(t *testing.T) {
	agent := func(mode ir.InteractionMode) *ir.AgentNode {
		n := &ir.AgentNode{}
		n.ID = "a"
		n.Interaction = mode
		return n
	}
	judge := func(mode ir.InteractionMode) *ir.JudgeNode {
		n := &ir.JudgeNode{}
		n.ID = "j"
		n.Interaction = mode
		return n
	}
	human := func(mode ir.InteractionMode) *ir.HumanNode {
		n := &ir.HumanNode{}
		n.ID = "h"
		n.Interaction = mode
		return n
	}

	cases := []struct {
		name string
		wf   *ir.Workflow
		want bool
	}{
		{"nil workflow", nil, false},
		{"no nodes", &ir.Workflow{}, false},
		{"agent none", &ir.Workflow{Nodes: map[string]ir.Node{"a": agent(ir.InteractionNone)}}, false},
		{"agent human", &ir.Workflow{Nodes: map[string]ir.Node{"a": agent(ir.InteractionHuman)}}, true},
		{"judge async", &ir.Workflow{Nodes: map[string]ir.Node{"j": judge(ir.InteractionAsync)}}, true},
		// Human nodes pause in the runtime — no MCP transport involved,
		// so they alone must not start the listener.
		{"human node only", &ir.Workflow{Nodes: map[string]ir.Node{"h": human(ir.InteractionHuman)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowHasInteractiveNode(tc.wf); got != tc.want {
				t.Fatalf("workflowHasInteractiveNode = %v, want %v", got, tc.want)
			}
		})
	}
}

// loopbackDriver satisfies just enough of sandbox.Driver (plus
// ProxyConfigurer) for startSandboxMCPListener: bind on loopback and
// advertise 127.0.0.1 so the test can actually dial the endpoint.
type loopbackDriver struct{}

func (loopbackDriver) Name() string                       { return "loopback-test" }
func (loopbackDriver) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }
func (loopbackDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return nil, nil
}
func (loopbackDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return nil, nil
}
func (loopbackDriver) ProxyConfig() (string, string, error) { return "127.0.0.1:0", "127.0.0.1", nil }

// The per-run ask-user listener must serve the MCP surface end-to-end:
// bind on the driver's gateway address, authorize the minted token, and
// answer initialize with a spec-complete serverInfo.
func TestStartSandboxMCPListener_ServesAskUser(t *testing.T) {
	token, err := askusermcp.NewRunToken()
	if err != nil {
		t.Fatalf("NewRunToken: %v", err)
	}
	endpoint, srv, err := startSandboxMCPListener(loopbackDriver{}, askusermcp.Handler(askusermcp.DefaultPath, token), askusermcp.DefaultPath)
	if err != nil {
		t.Fatalf("startSandboxMCPListener: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	if !strings.HasSuffix(endpoint, askusermcp.DefaultPath) {
		t.Fatalf("endpoint %q must end with %q", endpoint, askusermcp.DefaultPath)
	}

	post := func(tok string) *http.Response {
		req, _ := http.NewRequest("POST", endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		if tok != "" {
			req.Header.Set("X-Iterion-Run", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", endpoint, err)
		}
		return resp
	}

	// Wrong token → 401.
	resp := post("wrong")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: expected 401, got %d", resp.StatusCode)
	}

	// Minted token → initialize succeeds with serverInfo.version.
	resp = post(token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize: status=%d body=%s", resp.StatusCode, body)
	}
	var r struct {
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Result.ServerInfo.Version == "" {
		t.Fatal("initialize must carry serverInfo.version (claude-code Zod schema)")
	}
}
