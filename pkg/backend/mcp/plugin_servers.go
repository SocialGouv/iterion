package mcp

import (
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
