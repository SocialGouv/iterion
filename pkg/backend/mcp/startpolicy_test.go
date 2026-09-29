package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
