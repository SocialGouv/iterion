package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The per-node host-MCP token must not ride an environment variable.
//
// ITERION_PI_MCP_SERVERS carried the whole resolved server list — each http
// server's `headers` included, `X-Iterion-Run` among them — as an env VALUE,
// and a sandboxed spawn forwards env values on the container runtime's own
// host argv (`docker exec --env K=V`; the kubernetes driver prefixes the same
// values onto `env K=V`): /proc/<pid>/cmdline is mode 444, so every user of
// the host read the token for the whole session. Since #2758 that token
// spends the tenant's connector credentials, so what sat there was a
// credential. The env carries the catalog's PATH now, and the file is private
// (pi_mcp_config.go).
//
// Mutation that reddens these: put the JSON back into the extension env
// (piExtensionEnv), or make pinMCPServers return the path without writing the
// file behind it.

const piMCPProbeToken = "run-token-cccccccccccccccccccccccccccccccc"

// piMCPProbeTask is a node whose board and connector facades are wired: its
// resolved server list carries the token twice, in two servers' headers.
func piMCPProbeTask() Task {
	return Task{
		NodeID:                "probe",
		Iteration:             1,
		Capabilities:          []string{"board.create", connection.CapConnectors},
		BoardHTTPEndpoint:     "http://127.0.0.1:1/api/v1/mcp/board",
		ConnectorHTTPEndpoint: "http://127.0.0.1:1/api/v1/mcp/connectors",
		BoardRunToken:         piMCPProbeToken,
	}
}

// TestAHostPiSpawnCarriesTheCatalogPathAndNotItsContent: on the host the
// catalog is a 0600 file in a private directory, and the env the child gets
// names it — nothing else in that env can hold the token.
func TestAHostPiSpawnCarriesTheCatalogPathAndNotItsContent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	task := piMCPProbeTask()

	path, cleanup, err := pinMCPServers(context.Background(), task, "unused", nil)
	if err != nil {
		t.Fatalf("pinMCPServers: %v", err)
	}
	defer cleanup()

	if !strings.HasPrefix(path, tmp) {
		t.Fatalf("catalog path = %q, want a private file under %s", path, tmp)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the catalog back: %v", err)
	}
	if !strings.Contains(string(raw), piMCPProbeToken) {
		t.Errorf("the file lost the very token it exists to hide: %s", raw)
	}
	var servers []piMCPServerSpec
	if err := json.Unmarshal(raw, &servers); err != nil {
		t.Errorf("the file holds %q, want the resolved server list as JSON", raw)
	}
	if len(servers) != 2 {
		t.Errorf("servers = %d, want the board and the connector facade", len(servers))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — the file replaces an env value every user could read", info.Mode().Perm())
	}

	env := piExtensionEnv(task, nil, path)
	if got := env[piMCPServersFileEnv]; got != path {
		t.Errorf("env[%s] = %q, want the catalog's path", piMCPServersFileEnv, got)
	}
	for k, v := range env {
		if strings.Contains(v, piMCPProbeToken) {
			t.Errorf("the run token is still in the env, under %s: %q", k, v)
		}
	}

	// The pass that owns the spawn takes the file and its directory with it.
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the catalog file outlived the pass: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the catalog's directory outlived the pass: %v", err)
	}
}

// TestASandboxedPiSpawnReadsItsCatalogFromAnInContainerFile: the write is an
// exec of its own whose content travels on STDIN — the host argv of the
// `docker exec` that performs it carries nothing — and the file lands inside
// the container, named by the sweep that ends the spawn.
func TestASandboxedPiSpawnReadsItsCatalogFromAnInContainerFile(t *testing.T) {
	tmp := t.TempDir()
	// writerSandboxRun runs the real in-sandbox writer on the host, with the
	// destination rebased under the test's own temp dir.
	run := &writerSandboxRun{root: tmp}
	task := piMCPProbeTask()
	task.Sandbox = run
	mark := "probe"

	path, cleanup, err := pinMCPServers(context.Background(), task, mark, nil)
	if err != nil {
		t.Fatalf("pinMCPServers: %v", err)
	}
	defer cleanup()

	if path != sandboxPiMCPServersFile(mark) {
		t.Fatalf("catalog path = %q, want the in-container %s", path, sandboxPiMCPServersFile(mark))
	}
	if len(run.argvs) != 1 {
		t.Fatalf("the writer ran %d times, want once", len(run.argvs))
	}
	// The document must have travelled over stdin, never on the writer's own
	// argv: that argv is the `docker exec` client's, on the host, which is
	// the surface this whole move is about.
	if w := strings.Join(run.argvs[0], " "); strings.Contains(w, piMCPProbeToken) {
		t.Errorf("the writer carried the token on ITS argv: %q", w)
	}
	written, err := os.ReadFile(run.hostPath(path))
	if err != nil {
		t.Fatalf("read the in-sandbox catalog: %v", err)
	}
	if !strings.Contains(string(written), piMCPProbeToken) {
		t.Errorf("the file holds %q, want the resolved server list", written)
	}
	info, err := os.Stat(run.hostPath(path))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	// The spawn's cleanup list names it, or the file outlives the node.
	if !slices.Contains(piSandboxSpawnFiles(mark), path) {
		t.Error("piSandboxSpawnFiles omits the catalog, so the exec that ends the spawn leaves it in the container")
	}
}

// TestThePiSpawnEndNamesTheCatalogFile: the wiring, not the list alone — the
// exec that ends a sandboxed pi spawn must carry the file in its rm.
func TestThePiSpawnEndNamesTheCatalogFile(t *testing.T) {
	run := &recordingRun{}
	mark := "probe"
	end := killSandboxDelegateReporting(run, mark, sweepOwner{}, piSandboxSpawnFiles(mark)...)
	_ = end()
	if len(run.allArgv) == 0 {
		t.Fatal("the spawn end never ran an exec")
	}
	script := strings.Join(run.allArgv[0], " ")
	if !strings.Contains(script, shellquote.Quote(sandboxPiMCPServersFile(mark))) {
		t.Errorf("the spawn end's script never removes the catalog: %q", script)
	}
}

// TestThePiSpawnerHandsTheExtensionThePath: through the WIRING the sandboxed
// spawn actually uses — the spawner closure, which is where the extension's
// env is assembled — not the helper alone: a test of the helper is green
// while the env still carries the JSON.
func TestThePiSpawnerHandsTheExtensionThePath(t *testing.T) {
	run := &recordingRun{}
	task := piMCPProbeTask()
	task.Sandbox = run
	path := sandboxPiMCPServersFile("probe")

	spawn := (&PiRPCBackend{}).spawner(task, "pi", "probe", path)
	_ = spawn(context.Background(), []string{"--mode", "rpc"})

	env := run.gotOpts.Env
	if env == nil {
		t.Fatal("the spawn carried no env")
	}
	if got := env[piMCPServersFileEnv]; got != path {
		t.Errorf("env[%s] = %q, want the in-container catalog path", piMCPServersFileEnv, got)
	}
	for k, v := range env {
		if strings.Contains(v, piMCPProbeToken) {
			t.Errorf("the run token reached the container env under %s: %q", k, v)
		}
	}
}

// refusingExecRun is a sandbox whose every exec fails: the shape of a write
// that cannot land.
type refusingExecRun struct {
	sandbox.Run
}

func (r *refusingExecRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, errors.New("exec refused")
}

// TestAPiSpawnRefusesRatherThanFallBackToTheVariable: a catalog that cannot
// be written must fail the node, not go back to the env value — the fallback
// would be the leak, present exactly when something is already wrong.
func TestAPiSpawnRefusesRatherThanFallBackToTheVariable(t *testing.T) {
	task := piMCPProbeTask()
	task.Sandbox = &refusingExecRun{}
	path, cleanup, err := pinMCPServers(context.Background(), task, "probe", nil)
	if err == nil {
		t.Errorf("a sandbox that cannot be written to was accepted (path %q)", path)
	}
	if path != "" {
		t.Errorf("path = %q on refusal, want empty", path)
	}
	cleanup() // must be safe to call
}

// TestAPiNodeWithoutServersSetsNoVariable: no servers, no file, no write —
// and neither the old inline variable nor the path one appears.
func TestAPiNodeWithoutServersSetsNoVariable(t *testing.T) {
	run := &writerSandboxRun{root: t.TempDir()}
	task := Task{NodeID: "plain", Iteration: 1}
	task.Sandbox = run

	path, cleanup, err := pinMCPServers(context.Background(), task, "probe", nil)
	if err != nil {
		t.Fatalf("pinMCPServers: %v", err)
	}
	defer cleanup()
	if path != "" {
		t.Errorf("path = %q, want empty for a node with no servers", path)
	}
	if len(run.argvs) != 0 {
		t.Errorf("the writer ran %d times for a node with no servers", len(run.argvs))
	}
	for _, key := range []string{piMCPServersFileEnv, "ITERION_PI_MCP_SERVERS"} {
		if _, ok := piExtensionEnv(task, nil, path)[key]; ok {
			t.Errorf("env carries %s for a node with no servers", key)
		}
	}
}

// The RPC env builder passes the path through to the extension env, host side.
func TestPiRPCEnvCarriesTheCatalogPathOnly(t *testing.T) {
	env := piRPCEnv(context.Background(), piMCPProbeTask(), nil, "/tmp/iterion-pi-mcp-x/mcp-servers.json")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, piMCPProbeToken) {
		t.Error("the run token is still in the host spawn's env")
	}
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, piMCPServersFileEnv+"=") {
			found = true
		}
	}
	if !found {
		t.Errorf("env lost %s: %v", piMCPServersFileEnv, env)
	}
}
