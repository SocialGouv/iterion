package delegate

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/runops"
)

const runsMCPServerName = "iterion_runs"
const runsMCPSubcommand = "__mcp-runs"

func HasRunsReadCapability(caps []string) bool {
	for _, capability := range caps {
		if strings.TrimSpace(capability) == runops.CapRunsRead {
			return true
		}
	}
	return false
}

func (b *ClaudeCodeBackend) wireRunsMCP(task Task, opts []claudesdk.Option, extras *[]string) []claudesdk.Option {
	if !HasRunsReadCapability(task.Capabilities) {
		return opts
	}
	if task.Sandbox == nil && task.RunStoreDir != "" {
		selfPath := proc.LocateIterionBinary()
		if selfPath == "" {
			b.Logger.Warn("[%s#%d/claude-code] iterion binary not found; runs.read disabled", task.NodeID, task.Iteration)
			return opts
		}
		opts = append(opts, claudesdk.WithMCPServer(runsMCPServerName, &claudesdk.MCPStdioServer{
			Command: selfPath,
			Args:    []string{runsMCPSubcommand},
			Env: map[string]string{
				"ITERION_RUN_STORE_DIR": task.RunStoreDir,
				"ITERION_RUN_CAPS":      strings.Join(task.Capabilities, ","),
			},
		}))
	} else if task.RunsHTTPEndpoint != "" && task.BoardRunToken != "" {
		opts = append(opts, claudesdk.WithMCPServer(runsMCPServerName, &claudesdk.MCPHTTPServer{
			URL: task.RunsHTTPEndpoint,
			Headers: map[string]string{
				"X-Iterion-Run": task.BoardRunToken,
			},
			AlwaysLoad: true,
		}))
	} else {
		b.Logger.Warn("[%s#%d/claude-code] runs.read granted but no host transport is available", task.NodeID, task.Iteration)
		return opts
	}
	if len(task.AllowedTools) > 0 {
		*extras = append(*extras, RunToolsFor(task.Capabilities)...)
	}
	return opts
}

func RunToolsFor(caps []string) []string {
	c := runops.NewCapabilities(caps...)
	names := runops.ToolNamesFor(c)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "mcp__"+runsMCPServerName+"__"+name)
	}
	return out
}
