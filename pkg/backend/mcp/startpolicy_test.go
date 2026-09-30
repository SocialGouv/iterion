package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// spawnProbe is a stdio server config whose process, if it is ever started,
// creates a marker file. The MCP handshake then fails — the probe speaks no
// protocol — which is exactly the point: the question under test is whether
// the launcher SPAWNED it, not whether it works. An assertion on the returned
// error could not tell a refusal from a handshake failure; the marker can.
func spawnProbe(t *testing.T, name string, origin Origin) (*ServerConfig, func(time.Duration) bool) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "spawned")
	cfg := &ServerConfig{
		Name:      name,
		Origin:    origin,
		Transport: TransportStdio,
		Command:   "/bin/sh",
		Args:      []string{"-c", "touch " + marker + "; sleep 0.2"},
	}
	// The spawn is synchronous in exec.Command.Start, but the child's touch
	// is not, so the answer is time-bounded. Callers asserting "it started"
	// wait generously; callers asserting "it did not" wait only long enough
	// for a spawn that already happened to show — a refusal that never runs
	// the child must not cost that wait on every case.
	return cfg, func(within time.Duration) bool {
		deadline := time.Now().Add(within)
		for {
			if _, err := os.Stat(marker); err == nil {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func mustNotStart(t *testing.T, err error, spawned func(time.Duration) bool, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", what)
	}
	if !ServerNotStartable(err) {
		t.Fatalf("%s: expected a ServerNotStartableError, got %v", what, err)
	}
	if spawned(250 * time.Millisecond) {
		t.Fatalf("%s: the server's process was started despite the refusal", what)
	}
}

// Every path that can reach an MCP server converges on one start, and the
// policy has to hold on all of them. They are listed here by name rather than
// covered by one representative call: each is a separate entry point into the
// manager, and the one left ungated is the one that gets used.
func TestNoPathStartsARefusedServer(t *testing.T) {
	paths := map[string]func(t *testing.T, m *Manager, server string) error{
		"EnsureServers": func(t *testing.T, m *Manager, server string) error {
			return m.EnsureServers(context.Background(), tool.NewRegistry(), []string{server})
		},
		"ListResources": func(t *testing.T, m *Manager, server string) error {
			_, err := m.ListResources(context.Background(), server)
			return err
		},
		"ReadResource": func(t *testing.T, m *Manager, server string) error {
			_, err := m.ReadResource(context.Background(), server, "file:///x")
			return err
		},
	}

	for _, origin := range []Origin{OriginUnknown, OriginProject, OriginWorkflow} {
		for name, call := range paths {
			t.Run(origin.String()+"/"+name, func(t *testing.T) {
				cfg, spawned := spawnProbe(t, "probe", origin)
				m := NewManager(map[string]*ServerConfig{"probe": cfg},
					WithStartPolicy(StartOperatorServersOnly))
				mustNotStart(t, call(t, m, "probe"), spawned, name)
			})
		}
	}

	t.Run("the zero policy refuses too", func(t *testing.T) {
		cfg, spawned := spawnProbe(t, "probe", OriginProject)
		m := NewManager(map[string]*ServerConfig{"probe": cfg})
		mustNotStart(t, m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"probe"}), spawned,
			"unarmed manager")
	})

	t.Run("an operator server starts under the same policy", func(t *testing.T) {
		cfg, spawned := spawnProbe(t, "probe", OriginPlugin)
		m := NewManager(map[string]*ServerConfig{"probe": cfg},
			WithStartPolicy(StartOperatorServersOnly))
		// The handshake fails (the probe speaks no MCP), so an error is
		// expected — but it must not be a refusal, and the process must run.
		err := m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"probe"})
		if ServerNotStartable(err) {
			t.Fatalf("an operator server must not be refused: %v", err)
		}
		if !spawned(3 * time.Second) {
			t.Fatal("an operator server was never started")
		}
	})

	t.Run("StartAllServers starts a workflow server", func(t *testing.T) {
		cfg, spawned := spawnProbe(t, "probe", OriginWorkflow)
		m := NewManager(map[string]*ServerConfig{"probe": cfg}, WithStartPolicy(StartAllServers))
		if err := m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"probe"}); ServerNotStartable(err) {
			t.Fatalf("no policy was refusing here: %v", err)
		}
		if !spawned(3 * time.Second) {
			t.Fatal("an unsandboxed run must still start its workflow servers")
		}
	})
}

// The gate cannot live where the client is CREATED, because creating one
// starts nothing: the transport is dialled lazily on the first protocol
// operation, and on a tool-cache hit discovery registers closures over a
// client that was never started. Between that registration and the first tool
// call the run's sandbox settles — and a launcher that checked only at
// creation would then spawn the server it had just decided to refuse.
//
// This is the sequence, not a paraphrase of it: register under a permissive
// policy, tighten, then call the tool.
func TestTighteningAfterDiscoveryStopsTheDeferredSpawn(t *testing.T) {
	cfg, spawned := spawnProbe(t, "probe", OriginProject)
	cache := NewToolCache(t.TempDir(), time.Hour)
	// Seed the cache so discovery needs no live ListTools — the cache-hit
	// path is the one that registers over an unstarted client.
	if err := cache.Set("probe", cfg, []ToolInfo{{Name: "do_it", Description: "probe tool"}}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	registry := tool.NewRegistry()
	m := NewManager(map[string]*ServerConfig{"probe": cfg},
		WithToolCache(cache), WithStartPolicy(StartAllServers))
	if err := m.EnsureServers(context.Background(), registry, []string{"probe"}); err != nil {
		t.Fatalf("discovery from cache: %v", err)
	}
	if spawned(250 * time.Millisecond) {
		t.Fatal("a cache hit must not have started the server yet — the premise of this test is gone")
	}

	// The run turns out to be sandboxed.
	m.SetStartPolicy(StartOperatorServersOnly)

	td, err := registry.Resolve("mcp.probe.do_it")
	if err != nil {
		t.Fatalf("the tool was registered by discovery: %v", err)
	}
	_, callErr := td.Execute(context.Background(), []byte(`{}`))
	mustNotStart(t, callErr, spawned, "a registered closure after the policy tightened")
}

// A server whose start was refused at catalog-build time — a malformed auth
// block on a workflow-controlled server — fails its own use, and only that.
func TestAStartErrOnTheConfigRefusesWithoutSpawning(t *testing.T) {
	cfg, spawned := spawnProbe(t, "probe", OriginPlugin)
	cfg.StartErr = errors.New("malformed auth block")
	m := NewManager(map[string]*ServerConfig{"probe": cfg}, WithStartPolicy(StartAllServers))

	err := m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"probe"})
	if err == nil || !errors.Is(err, cfg.StartErr) {
		t.Fatalf("the recorded reason must travel to the caller, got %v", err)
	}
	if spawned(250 * time.Millisecond) {
		t.Fatal("a server that cannot start must not be spawned")
	}
}

// The health check connects — for a stdio server, spawns — so it has to obey
// the same policy. Skipping is not an error: a CLI backend starts that server
// in the container later.
func TestHealthCheckSkipsWhatItMayNotStart(t *testing.T) {
	refused, refusedSpawned := spawnProbe(t, "refused", OriginProject)
	m := NewManager(map[string]*ServerConfig{"refused": refused},
		WithStartPolicy(StartOperatorServersOnly))

	if err := m.HealthCheck(context.Background(), []string{"refused"}); err != nil {
		t.Fatalf("a skipped server is not a health-check failure: %v", err)
	}
	if refusedSpawned(250 * time.Millisecond) {
		t.Fatal("the health check started a server the policy refuses")
	}
}

// Tightening mid-run also has to stop what is ALREADY running: the process is
// beside the launcher, and nothing else would end it before the run does.
func TestTighteningClosesAnAlreadyStartedRefusedServer(t *testing.T) {
	cfg, spawned := spawnProbe(t, "probe", OriginProject)
	m := NewManager(map[string]*ServerConfig{"probe": cfg}, WithStartPolicy(StartAllServers))
	_ = m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"probe"})
	if !spawned(3 * time.Second) {
		t.Fatal("the premise of this test is that the server did start")
	}

	m.SetStartPolicy(StartOperatorServersOnly)

	state, err := m.state("probe")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	state.mu.Lock()
	client := state.client
	discovered := state.discovered
	state.mu.Unlock()
	if client != nil {
		t.Error("the refused server's client must be dropped, not kept for the next call")
	}
	if discovered {
		t.Error("a refused server must not stay marked as discovered")
	}
}

// slowHelperMode reports whether this process was re-executed as the slow MCP
// server below. The delay is what lets a test tighten the policy while a start
// is genuinely IN FLIGHT — the window the gate on the way in cannot see.
func slowHelperMode() bool {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) && os.Args[i+1] == "mcp-slow-helper" {
			return true
		}
	}
	return false
}

// runSlowStdioHelper writes its PID where the test can watch it, waits, then
// speaks real MCP. A peer that never answers would fail the start outright and
// exercise nothing: the case under test is a start that SUCCEEDS after the
// launcher has been told it may not run this server.
func runSlowStdioHelper() {
	if path := os.Getenv("ITERION_TEST_MCP_PIDFILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	time.Sleep(700 * time.Millisecond)
	server := gomcp.NewServer(&gomcp.Implementation{Name: "slow-server", Version: "v0.0.1"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "noop", Description: "does nothing"},
		func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "ok"}}}, nil, nil
		})
	if err := server.Run(context.Background(), &gomcp.StdioTransport{}); err != nil {
		// Not 1: this process IS a test binary re-executed as a helper, and
		// 1 is what `go test` exits with on a failing test. A CI log showing
		// 1 here says nothing about which of the two happened.
		os.Exit(slowHelperServeFailure)
	}
	os.Exit(0)
}

// slowHelperServeFailure is the helper's own exit code, distinct from the
// test binary's.
const slowHelperServeFailure = 97

// A start is slow — a process spawn, a dial, a handshake — and the run's
// sandbox settles while it is in flight. Checking the policy only on the way
// IN therefore protects nothing at the one moment that matters: the launcher
// learns it may not run this server, and the server is already coming up.
//
// What used to happen: the manager closed a session that did not exist yet
// (nil), dropped its only reference to the client, and the start then
// published a live session into it. The refused server's process ran beside
// the launcher for the rest of the run and past it, reachable by nobody —
// Manager.Close could not see it either.
func TestTighteningWhileAStartIsInFlightLeavesNoProcessBehind(t *testing.T) {
	if slowHelperMode() {
		runSlowStdioHelper()
		return
	}
	pidfile := filepath.Join(t.TempDir(), "pid")
	cfg := &ServerConfig{
		Name: "slow", Origin: OriginProject, Transport: TransportStdio,
		Command: os.Args[0],
		Args: []string{"-test.run=TestTighteningWhileAStartIsInFlightLeavesNoProcessBehind",
			"--", "mcp-slow-helper"},
		Env: map[string]string{"ITERION_TEST_MCP_PIDFILE": pidfile},
	}
	m := NewManager(map[string]*ServerConfig{"slow": cfg}, WithStartPolicy(StartAllServers))
	t.Cleanup(func() { _ = m.Close() })

	done := make(chan error, 1)
	go func() {
		_, err := m.ListResources(context.Background(), "slow")
		done <- err
	}()

	// Wait until the child is actually up, so the tightening lands INSIDE the
	// handshake rather than before the spawn (which the entry gate already
	// covers, and which would make this test pass for the wrong reason).
	pid := waitForPID(t, pidfile)
	m.SetStartPolicy(StartOperatorServersOnly)

	err := <-done
	if !ServerNotStartable(err) {
		t.Fatalf("the call must be refused once the policy tightened, got %v", err)
	}
	if alive, why := processAliveWithin(pid, 5*time.Second); alive {
		t.Errorf("pid %d (origin %s) is still running beside the launcher after the refusal: %s",
			pid, cfg.Origin, why)
	}

	state, stateErr := m.state("slow")
	if stateErr != nil {
		t.Fatalf("state: %v", stateErr)
	}
	state.mu.Lock()
	client := state.client
	state.mu.Unlock()
	if client != nil {
		t.Error("a refused server must not be left with a usable client")
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper server never reported its pid — the premise of this test is gone")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAliveWithin polls for the process to disappear, so a slow exit does
// not read as a leak. It reports whether the process is still there and, when
// it is, why we know.
//
// Only ESRCH is "gone". EPERM says the opposite — the process EXISTS and this
// uid may not signal it — and reading it as gone is how the leak this test
// guards would report as absent: a process running beside the launcher,
// reachable by nobody, is exactly the state that answers EPERM once its pid
// belongs to someone else. Any other errno is unexpected and says so rather
// than being folded into either answer.
func processAliveWithin(pid int, within time.Duration) (alive bool, why string) {
	deadline := time.Now().Add(within)
	for {
		err := syscall.Kill(pid, 0)
		switch {
		case err == nil:
			if time.Now().After(deadline) {
				return true, "signalable"
			}
		case errors.Is(err, syscall.ESRCH):
			return false, ""
		case errors.Is(err, syscall.EPERM):
			return true, "alive but owned by another uid (EPERM) — the pid was recycled, or the child outlived us"
		default:
			return true, "kill(pid, 0): " + err.Error()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A reason the server could not have started anyway — a malformed auth block
// recorded when the catalog was built — must not swallow the placement
// answer. The callers branch on the TYPE to choose between refusing the node
// (and walking its fallbacks) and failing it outright; an untyped error there
// kills a node that a `claude_code` route could still have served, for a
// server whose definition came from the repository under review.
func TestARecordedReasonTravelsInsideTheRefusalNotInsteadOfIt(t *testing.T) {
	broken := errors.New("oauth: server \"repo\" AuthURL: must be https")

	t.Run("under a sandbox the refusal wins and carries the reason", func(t *testing.T) {
		cfg, spawned := spawnProbe(t, "repo", OriginProject)
		cfg.StartErr = broken
		m := NewManager(map[string]*ServerConfig{"repo": cfg}, WithStartPolicy(StartOperatorServersOnly))

		err := m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"repo"})
		if !ServerNotStartable(err) {
			t.Fatalf("expected the typed refusal, got %v", err)
		}
		if !errors.Is(err, broken) {
			t.Errorf("the recorded reason must still be reachable: %v", err)
		}
		if spawned(250 * time.Millisecond) {
			t.Error("nothing should have been spawned")
		}
	})

	t.Run("with nothing to refuse it is a plain failure", func(t *testing.T) {
		cfg, _ := spawnProbe(t, "firecrawl", OriginPlugin)
		cfg.StartErr = broken
		m := NewManager(map[string]*ServerConfig{"firecrawl": cfg}, WithStartPolicy(StartAllServers))

		err := m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"firecrawl"})
		if ServerNotStartable(err) {
			t.Fatalf("no policy refused this server; it is simply broken: %v", err)
		}
		if !errors.Is(err, broken) {
			t.Errorf("the reason must reach the caller: %v", err)
		}
	})
}

// The health check exists to fail fast on a misconfigured catalog. It skips
// what the launcher may not start — the probe IS a connection — but a server
// known to be broken for any OTHER reason is precisely what it must report.
func TestHealthCheckStillReportsAServerItMayStart(t *testing.T) {
	cfg, _ := spawnProbe(t, "firecrawl", OriginPlugin)
	cfg.StartErr = errors.New("malformed auth block")
	m := NewManager(map[string]*ServerConfig{"firecrawl": cfg}, WithStartPolicy(StartAllServers))

	if err := m.HealthCheck(context.Background(), []string{"firecrawl"}); err == nil {
		t.Error("a health check that hides a server it knows cannot start reads as a clean bill of health")
	}
}

// claw's mcp_auth and the resource pair ask the provider about a server by
// name, and the provider answered for a refused one exactly as for a live
// one: "connected", with the resolved command — the operator's own filesystem
// layout — handed to a conversation that was just refused that server.
func TestTheProviderDoesNotVouchForAServerItMayNotStart(t *testing.T) {
	cfg := &ServerConfig{
		Name: "repo", Origin: OriginProject, Transport: TransportStdio,
		Command: "/opt/operator/private-path/mcp-server",
	}
	m := NewManager(map[string]*ServerConfig{"repo": cfg}, WithStartPolicy(StartOperatorServersOnly))
	p := m.ClawProvider(nil)

	status, ok := p.ServerStatus("repo")
	if !ok {
		t.Fatal("the server is in the catalog; the model should get an answer, not a lookup miss")
	}
	if status.Status == "connected" {
		t.Error("a server the launcher may not start is not connected")
	}
	if strings.Contains(status.ServerInfo, cfg.Command) {
		t.Error("the refusal must not disclose the resolved command")
	}
	// A client IS handed back: `ok=false` is claw's "server not found", which
	// is the one answer that is false — and the one that invites the model to
	// retry name variants. The refusal has to come from the operation, with
	// its own reason, and before any dial.
	client, got := p.GetResourceClient("repo")
	if !got {
		t.Fatal("the server exists; answering \"not found\" sends the model looking for a name")
	}
	if _, err := client.ListResources(context.Background()); !ServerNotStartable(err) {
		t.Errorf("the operation must carry the typed refusal, got %v", err)
	}
	if _, err := client.ReadResource(context.Background(), "file:///x"); !ServerNotStartable(err) {
		t.Errorf("read must carry the typed refusal too, got %v", err)
	}

	// The same provider still serves what the operator installed.
	allowed := &ServerConfig{Name: "firecrawl", Origin: OriginPlugin, Transport: TransportStdio, Command: "npx"}
	m2 := NewManager(map[string]*ServerConfig{"firecrawl": allowed}, WithStartPolicy(StartOperatorServersOnly))
	p2 := m2.ClawProvider(nil)
	if status, ok := p2.ServerStatus("firecrawl"); !ok || status.Status != "connected" {
		t.Errorf("an operator server must still report connected: ok=%v status=%+v", ok, status)
	}
	if _, ok := p2.GetResourceClient("firecrawl"); !ok {
		t.Error("an operator server must still yield a client")
	}
}

// A refusal is the answer the policy gave at one moment, not a property of
// the server — and the policy changes: the engine RELAXES it when a run
// settles without a sandbox, which is the whole point of settling.
//
// Both halves of the relaxation are asserted, because round 1 broke both
// while fixing the tightening: a refusal cached as a permanent start failure
// (so the client refused forever from a policy that no longer existed), and
// tools left in the registry by a tightening that cleared `discovered` (so
// re-discovery died on its own leftovers, with an error saying the server
// "cannot boot" — which kills the node before its fallbacks).
func TestARefusedServerWorksAgainOnceThePolicyRelaxes(t *testing.T) {
	if slowHelperMode() {
		runSlowStdioHelper()
		return
	}
	cfg := &ServerConfig{
		Name: "swings", Origin: OriginProject, Transport: TransportStdio,
		Command: os.Args[0],
		Args: []string{"-test.run=TestARefusedServerWorksAgainOnceThePolicyRelaxes",
			"--", "mcp-slow-helper"},
	}
	registry := tool.NewRegistry()
	m := NewManager(map[string]*ServerConfig{"swings": cfg}, WithStartPolicy(StartAllServers))
	t.Cleanup(func() { _ = m.Close() })

	// Discovered and registered while the launcher was allowed to start it.
	if err := m.EnsureServers(context.Background(), registry, []string{"swings"}); err != nil {
		t.Fatalf("premise: the server discovers under StartAllServers: %v", err)
	}
	if len(registry.ListByServer("swings")) == 0 {
		t.Fatal("premise: discovery registered its tools")
	}

	// The run turns out to be sandboxed.
	m.SetStartPolicy(StartOperatorServersOnly)
	if err := m.EnsureServers(context.Background(), registry, []string{"swings"}); !ServerNotStartable(err) {
		t.Fatalf("while sandboxed the server must be refused, got %v", err)
	}

	// …and then the sandbox turns out not to have started (an `auto` run on a
	// host with no container runtime): the engine says so, and the server is
	// startable again.
	m.SetStartPolicy(StartAllServers)
	if err := m.EnsureServers(context.Background(), registry, []string{"swings"}); err != nil {
		t.Fatalf("after the policy relaxed the server must work again: %v", err)
	}
	if len(registry.ListByServer("swings")) == 0 {
		t.Error("its tools must be back in the registry")
	}
}

// The far-side gate writes its refusal into the client's cached startErr, and
// the next attempt reads that cache through a rule meant for PERMANENT
// failures ("don't retry"). A refusal is not permanent: it is what the policy
// said at one instant, and the engine relaxes the policy when a run settles
// without a sandbox.
//
// The client that keeps the poison is the one a registered tool closure
// captured — the manager drops its own reference when it tightens. So the
// symptom is a tool that is advertised, has a live server behind it, and
// refuses for the rest of the run from a policy that no longer exists.
//
// Deterministic by construction: the gate here answers from a variable this
// test sets, refusing on the FAR side of one start (the entry check passes,
// the post-start check does not) and allowing afterwards.
func TestAFarSideRefusalIsNotCachedAsAPermanentFailure(t *testing.T) {
	mcpServer := gomcp.NewServer(&gomcp.Implementation{Name: "far-side", Version: "v0.0.1"}, nil)
	gomcp.AddTool(mcpServer, &gomcp.Tool{
		Name: "noop", Description: "No-op", InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input any) (*gomcp.CallToolResult, any, error) {
		return &gomcp.CallToolResult{}, nil, nil
	})
	handler := gomcp.NewStreamableHTTPHandler(
		func(r *http.Request) *gomcp.Server { return mcpServer },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	peer := httptest.NewServer(handler)
	defer peer.Close()

	cfg := &ServerConfig{Name: "far", Origin: OriginProject, Transport: TransportHTTP, URL: peer.URL}
	// calls: 1 = the entry check of the first start (allow, so the start
	// runs), 2 = the post-start check (refuse — the policy tightened while
	// the peer was handshaking), 3+ = the policy relaxed again.
	calls := 0
	gate := func() error {
		calls++
		if calls == 2 {
			return &ServerNotStartableError{Server: "far", Origin: OriginProject, Policy: StartOperatorServersOnly}
		}
		return nil
	}
	client := newSDKClient(cfg, clientInfo{Name: "test", Version: "1"}, gate, nil)

	if _, err := client.ListTools(context.Background()); !ServerNotStartable(err) {
		t.Fatalf("premise: the far-side gate refuses this start, got %v", err)
	}
	// Same client — the one a registered tool closure would still be holding.
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("after the policy relaxed this client must work again, got %v", err)
	}
}
