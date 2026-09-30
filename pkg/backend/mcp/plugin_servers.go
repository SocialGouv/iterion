package mcp

import (
	"fmt"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// loadPluginServers loads the plugin registry and returns the MCP servers
// contributed by every enabled plugin, keyed by server name, with activation-
// time placeholders ({{workspace}}, {{plugin.dir}}, {{plugin.cache}}) expanded
// against the given workspace. A registry-load failure yields an empty map —
// a broken plugin must never break MCP setup for a run. This is the bridge
// between the plugin "mcp" contribution kind and the existing MCP catalog.
//
// Origin is OriginPlugin only for a plugin whose CODE is the operator's — a
// builtin, or a manifest under the iterion home the inherited environment
// names (Registry.OperatorControlled). A plugin loaded from a home a project
// `.env` selected keeps every other behaviour and loses only that authority:
// its servers are OriginUnknown, so the launcher will not start them while a
// sandbox is active. Without the distinction, a repository carrying `.env` and
// a `default_enabled` manifest would classify its own code as the operator's.

// PluginServersUnavailable returns the reason plugin MCP servers are missing
// from a workflow, or nil when the registry loaded whole.
//
// Two reasons, not one. The registry can fail outright, and it can load while
// SKIPPING an individual plugin whose manifest it could not parse — the
// second is the one that actually happens, and it takes that plugin's servers
// off every node while `plugin.Load()` returns no error at all.
// Registry.loadInstalled records those in LoadSkips precisely so a consumer
// who needs to have seen every plugin can say so; pkg/runtime already vetoes
// its contribution mirror on them.
//
// PrepareWorkflow reports this through its optional logger, which the run
// path passes and the read-only analyses (`iterion validate`, the studio
// compile) have none of. Those surfaces ask here instead and put the answer
// where their user looks — a validation whose tool list is silently missing
// a plugin's MCP servers reads as "that plugin contributes none".
func PluginServersUnavailable() error {
	reg, err := plugin.Load()
	if err != nil {
		return fmt.Errorf("mcp: plugin registry failed to load — NO plugin MCP server is available: %w", err)
	}
	if skips := reg.LoadSkips(); len(skips) > 0 {
		return fmt.Errorf("mcp: %d installed plugin(s) were skipped while loading, so their MCP servers are "+
			"absent: %s", len(skips), strings.Join(skips, "; "))
	}
	return nil
}

func loadPluginServers(workspace string, logger *iterlog.Logger) map[string]*ServerConfig {
	reg, err := plugin.Load()
	if err != nil {
		// Every plugin's MCP servers disappear from every node of the run.
		// Silence made that indistinguishable from "no plugins are enabled" —
		// and the file that most often breaks the load, <home>/plugins.yaml,
		// is the repository's own whenever a project `.env` selected the home.
		logger.Warn("mcp: plugin registry failed to load — NO plugin MCP server is available to this run: %v", err)
		return map[string]*ServerConfig{}
	}
	// A registry that loaded but skipped a plugin is the common half of the
	// same silence: that plugin's servers are gone from every node of the
	// run, and the load returned no error to say so.
	if skips := reg.LoadSkips(); len(skips) > 0 {
		logger.Warn("mcp: %d installed plugin(s) skipped while loading — their MCP servers are NOT available "+
			"to this run: %s", len(skips), strings.Join(skips, "; "))
	}
	out := map[string]*ServerConfig{}
	for _, p := range reg.Enabled() {
		exp := reg.ExpandContextFor(p.Name(), workspace)
		origin := OriginUnknown
		if reg.OperatorControlled(p) {
			origin = OriginPlugin
		}
		for _, s := range p.Manifest.Contributes.MCPServers {
			transport := Transport(s.Transport)
			if transport == "" {
				transport = TransportStdio
			}
			args := make([]string, 0, len(s.Args))
			for _, a := range s.Args {
				args = append(args, exp.Expand(a))
			}
			env := map[string]string{}
			for k, v := range s.Env {
				env[k] = exp.Expand(v)
			}
			out[s.Name] = &ServerConfig{
				Name:      s.Name,
				Origin:    origin,
				Transport: transport,
				Command:   exp.Expand(s.Command),
				Args:      args,
				URL:       exp.Expand(s.URL),
				Headers:   s.Headers,
				Env:       env,
			}
		}
	}
	return out
}
