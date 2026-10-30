package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The pi spawn's MCP catalog travels as a FILE, never inline in an
// environment variable.
//
// ITERION_PI_MCP_SERVERS carried the whole resolved server list as one JSON
// value, each http server's `headers` included — `X-Iterion-Run`, the
// per-node host-MCP token, among them. Since #2758 that token spends the
// tenant's connector credentials, so it is a credential in its own right, and
// an env value is world-readable for the whole life of the session: the
// sandboxed spawn forwards it as `docker exec --env K=V`, which rides the
// host docker client's own argv — /proc/<pid>/cmdline is mode 444 — and the
// kubernetes driver prefixes the same values onto `env K=V` argv. The host
// spawn kept it in the child's environ, inherited by everything the node
// runs.
//
// This is the move claude_code's MCP catalog made (claude_code_mcp_config.go)
// for the same transport, applied to its pi half. The reader is first-party:
// pi has no MCP client, and the iterion extension loaded through `-e` bridges
// every server, so the extension takes the path from
// ITERION_PI_MCP_SERVERS_FILE and reads the document from the file — the
// inline spelling is gone from both sides. Engine and extension ship in one
// binary (piext), so no mixed pairing exists to migrate; a stale variable is
// inert, and the platform config keeps both names refused as bot vars.
//
// A write that fails REFUSES the spawn rather than falling back to the
// variable: a fallback would be the leak, present exactly when something is
// already wrong.

// piMCPServersFileEnv is the variable that carries the catalog's PATH. The
// value is not a secret: it names a file only the extension reads.
const piMCPServersFileEnv = "ITERION_PI_MCP_SERVERS_FILE"

// piMCPServersFileName is the basename of the private file a spawn's MCP
// catalog is written to, on the host and in a sandbox alike.
const piMCPServersFileName = "mcp-servers.json"

// sandboxPiMCPServersFile is the in-container path of a sandboxed spawn's MCP
// catalog. /tmp is writable in every sandbox image whatever the pinned User —
// the same ground sandboxDelegatePIDFile stands on.
func sandboxPiMCPServersFile(mark string) string {
	return "/tmp/iterion-delegate-" + mark + "." + piMCPServersFileName
}

// piSandboxSpawnFiles lists every private file a sandboxed pi spawn writes
// inside the container, so the exec that ends the spawn removes all of them
// (killSandboxDelegate's file list). ONE list, read by both the write and the
// sweep: a file added here is cleaned up without a second site remembering.
func piSandboxSpawnFiles(mark string) []string {
	return []string{sandboxPiMCPServersFile(mark)}
}

// pinMCPServers resolves the node's MCP servers once and parks the document
// where only the extension reads it, returning the PATH the env must carry —
// empty when the node has no servers — and the cleanup that removes what was
// written.
//
// Sandboxed: the file is written INSIDE the container through
// sandbox.WriteFileExec, whose content travels on the writer's STDIN and
// never on an argv or an env value — and a host /tmp file is invisible to the
// extension running under `docker exec`, which is what C082 was paid for.
// The sweep that ends the spawn removes it (piSandboxSpawnFiles). Host: a
// 0600 file in a 0700 directory of its own, removed when the node ends.
func pinMCPServers(ctx context.Context, task Task, mark string, logger *iterlog.Logger) (string, func(), error) {
	servers := piMCPServers(task, logger)
	if len(servers) == 0 {
		return "", func() {}, nil
	}
	raw, err := json.Marshal(servers)
	if err != nil {
		return "", func() {}, fmt.Errorf("pi: encode the node's MCP servers: %w", err)
	}
	if task.Sandbox != nil {
		path := sandboxPiMCPServersFile(mark)
		if err := sandbox.WriteFileExec(ctx, task.Sandbox, path, raw); err != nil {
			return "", func() {}, fmt.Errorf("pi: write the MCP catalog inside the sandbox: %w", err)
		}
		return path, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "iterion-pi-mcp-")
	if err != nil {
		return "", func() {}, fmt.Errorf("pi: create the private directory for the MCP catalog: %w", err)
	}
	path := filepath.Join(dir, piMCPServersFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		// Nothing else was created, and the node is refused: take the
		// directory back rather than leave it for the run.
		os.Remove(path)
		os.Remove(dir)
		return "", func() {}, fmt.Errorf("pi: write the private MCP catalog file: %w", err)
	}
	return path, func() {
		for _, p := range []string{path, dir} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && logger != nil {
				logger.Warn("[%s#%d/%s] remove the MCP catalog file %s: %v",
					task.NodeID, task.Iteration, BackendPi, p, err)
			}
		}
	}, nil
}
