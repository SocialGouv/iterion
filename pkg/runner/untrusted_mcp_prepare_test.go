package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestExecuteRun_TheCheckoutsMCPServersFollowTheMessagesTrust.
//
// The row of the carrier bench (e2e/untrusted_workspace_guarantee_test.go,
// TestUntrustedWorkspace_EveryCarrierSiteIsWatched) that cannot live there:
// executeRun is reached in production only through a NATS consumer, and the
// e2e package has no queue. So it is driven HERE, the way the rest of this
// package drives it — a Runner built on a store, handed the RunMessage — which
// still exercises the site itself: the PrepareWorkflow call inside executeRun,
// fed the message's trust.
//
// The checkout ships a `.mcp.json` whose server the workflow activates by name.
// On the trusted control it resolves and the run goes on; on a fork the
// checkout's catalog is not read, so the activation is refused, typed and with
// the reason — before any node could spawn it.
func TestExecuteRun_TheCheckoutsMCPServersFollowTheMessagesTrust(t *testing.T) {
	src := strings.ReplaceAll(`dsl: 2

tool t:
  command: __TRUE__
workflow main:
  entry: t
  mcp:
    servers: [proj]
  t -> done
`, "__TRUE__", "`exit 0`")
	pr, perr := parser.Parse("proj.bot", src)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}

	for _, row := range []struct {
		name     string
		trust    store.RunTrust
		withheld bool
	}{
		{"a trusted checkout's server resolves", store.RunTrustDefault, false},
		{"a fork's server is not loaded", store.RunTrustFork, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, ".mcp.json"),
				[]byte(`{"mcpServers":{"proj":{"command":"true"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			runID := "run-mcp-" + string(row.trust)
			if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: ws, Logger: iterlog.New(iterlog.LevelWarn, nil)}}
			msg := &queue.RunMessage{RunID: runID, WorkflowName: "main", IRCompiled: body, Trust: row.trust}

			execErr := r.executeRun(ctx, msg, nil, nil)
			refused := errors.Is(execErr, mcp.ErrWithdrawnMCPServer) && strings.Contains(execErr.Error(), `"proj"`)
			if refused != row.withheld {
				t.Fatalf("withheld = %v, want %v (executeRun: %v)", refused, row.withheld, execErr)
			}
			if !row.withheld && execErr != nil && strings.Contains(execErr.Error(), "resolve MCP servers") {
				t.Fatalf("the TRUSTED control did not get past the MCP resolution — this row proves nothing: %v", execErr)
			}
			// The refusal is on the run's timeline too — the engine that would
			// otherwise say what was withheld never starts.
			events, _ := st.LoadEvents(ctx, runID)
			refusals := 0
			for _, e := range events {
				if e.Type == store.EventWorkspaceTrustWithdrawal && e.Data["refused"] == true {
					refusals++
				}
			}
			if row.withheld && refusals != 1 {
				t.Errorf("refusal events on the timeline = %d, want 1", refusals)
			}
			if !row.withheld && refusals != 0 {
				t.Errorf("a trusted run recorded %d refusal events", refusals)
			}
		})
	}
}

// TestExecuteRun_APluginsMCPServerIsNotBootedForAFork.
//
// The plugin half of the same floor, driven through executeRun — the one path
// a fork run takes in production — with a REAL plugin enabled under
// ITERION_HOME. Its server is the operator's configuration, but it is booted
// where the agent runs, and its launcher reads its own configuration from the
// checkout. The server's command writes a canary when it starts.
//
// The two rows drive the run differently, each where the boot would happen:
// the TRUSTED control is a claw agent node against a stub model — the
// executor splices the ambient server into the node's tool set, the boot
// exits 1 on purpose, and the node carries on without its tools — and the
// canary proves the boot was attempted. The fork row is admitted only
// sandboxed (#1773), and a sandboxed claw node would spawn `iterion
// __claw-runner` from PATH, a binary this test does not control — so it runs
// a tool node instead: the floor is decided at MCP preparation, the run
// completes without the server, and says so before any node runs — one
// workspace_trust_withdrawal event naming the server and its plugin.
func TestExecuteRun_APluginsMCPServerIsNotBootedForAFork(t *testing.T) {
	// A fork run is admitted only sandboxed (#1773): the fork workflow asks
	// for a container and the runner is handed a driver that answers
	// (hostDriver below — its commands run on the host, which is what a
	// test's stub driver can promise).
	forkSrc := strings.ReplaceAll(`dsl: 2

tool t:
  command: __TRUE__

workflow main:
  entry: t
  sandbox: auto
  t -> done
`, "__TRUE__", "`exit 0`")
	trustedSrc := `dsl: 2

prompt p:
  Review the diff.

agent a:
  backend: "claw"
  model: "anthropic/claude-bench"
  tools: [read_file]
  user: p

workflow main:
  entry: a
  a -> done
`
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		src   string
	}{
		{"trusted", store.RunTrustDefault, trustedSrc},
		{"fork", store.RunTrustFork, forkSrc},
	} {
		t.Run(row.name, func(t *testing.T) {
			pr, perr := parser.Parse("plugin.bot", row.src)
			if perr != nil {
				t.Fatalf("parse: %v", perr)
			}
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("parse error: %s", d.Error())
				}
			}
			body, err := ast.MarshalFile(pr.File)
			if err != nil {
				t.Fatalf("marshal AST: %v", err)
			}
			home := t.TempDir()
			canary := filepath.Join(t.TempDir(), "plugin-server-started")
			pluginDir := filepath.Join(home, "plugins", "probe")
			if err := os.MkdirAll(pluginDir, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := "name: probe\nversion: 1.0.0\ndescription: the operator's indexer\nschema_version: 1\ndefault_enabled: true\n" +
				"contributes:\n  mcp_servers:\n    - name: plug\n      transport: stdio\n      command: /bin/sh\n      args:\n        - -c\n        - \"echo started >> '" + canary + "'; exit 1\"\n"
			if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ITERION_HOME", home)
			// Pin the host kind so the factory's preference walk consults the
			// driver registry below (a CI pod's cloud order never asks it).
			t.Setenv("ITERION_MODE", "local")

			if row.trust == store.RunTrustDefault {
				model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					for _, e := range []string{
						`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-bench","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
						`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
						`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
						`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
						`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
						`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
					} {
						_, _ = io.WriteString(w, e+"\n\n")
					}
				}))
				defer model.Close()
				t.Setenv("ANTHROPIC_BASE_URL", model.URL)
				t.Setenv("ANTHROPIC_API_KEY", "bench-key")
				for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ZAI_API_KEY", "OPENAI_API_KEY"} {
					t.Setenv(k, "")
				}
			}

			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			runID := "run-plugin-" + row.name
			if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{
				Store:   st,
				WorkDir: t.TempDir(),
				Logger:  iterlog.New(iterlog.LevelWarn, nil),
				// The admission floor (#1773) refuses a fork run whose sandbox
				// resolves to no container; this registry answers its probe.
				SandboxDrivers: map[string]sandbox.DriverConstructor{
					"docker": func() (sandbox.Driver, error) { return hostDriver{}, nil },
				},
			}}
			msg := &queue.RunMessage{RunID: runID, WorkflowName: "main", IRCompiled: body, Trust: row.trust}
			execErr := r.executeRun(ctx, msg, nil, nil)

			_, statErr := os.Stat(canary)
			started := statErr == nil
			events, _ := st.LoadEvents(ctx, runID)
			var withdrawals []*store.Event
			for _, e := range events {
				if e.Type == store.EventWorkspaceTrustWithdrawal {
					withdrawals = append(withdrawals, e)
				}
			}
			t.Logf("executeRun=%v server_started=%v withdrawals=%d", execErr, started, len(withdrawals))
			if execErr != nil {
				t.Fatalf("the run failed: %v — the node must run on without the plugin's tools", execErr)
			}
			if row.trust == store.RunTrustDefault {
				if !started {
					t.Fatal("the TRUSTED control never started the plugin's server — this row proves nothing")
				}
				if len(withdrawals) != 0 {
					t.Errorf("a trusted run announced a withdrawal: %+v", withdrawals)
				}
				return
			}
			if started {
				t.Error("a fork run started the plugin's MCP server")
			}
			if len(withdrawals) != 1 || withdrawals[0].NodeID != "" || withdrawals[0].Data["withdrawn"] != "mcp_servers" {
				t.Fatalf("want ONE run-level mcp_servers withdrawal, got %+v", withdrawals)
			}
			raw, _ := json.Marshal(withdrawals[0].Data["items"])
			if !strings.Contains(string(raw), "plug (plugin probe)") {
				t.Errorf("the withdrawal does not name the server and its plugin: %s", raw)
			}
		})
	}
}

// hostDriver is the sandbox a runner test can promise: it answers the
// admission probe with a container's name, and its commands run on the host —
// the shape pkg/runtime's podDriver has, duplicated here because test helpers
// do not cross packages.
type hostDriver struct{}

func (hostDriver) Name() string { return "docker" }
func (hostDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true}
}

type hostPrepared struct{}

func (hostPrepared) DriverName() string { return "docker" }

func (hostDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return hostPrepared{}, nil
}

func (hostDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return hostRun{}, nil
}

type hostRun struct{}

func (hostRun) Driver() string { return "docker" }
func (hostRun) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}
func (hostRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := hostRun{}.Command(ctx, argv, opts)
	c.Stdin = opts.Stdin
	return sandbox.ExecCmd(c, opts)
}
func (hostRun) Cleanup(context.Context) error { return nil }
