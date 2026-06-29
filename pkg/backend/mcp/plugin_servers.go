package mcp

import (
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// loadPluginServers loads the plugin registry and returns the MCP servers
// contributed by every enabled plugin, keyed by server name, with activation-
// time placeholders ({{workspace}}, {{plugin.dir}}, {{plugin.cache}}) expanded
// against the given workspace. A registry-load failure yields an empty map —
// a broken plugin must never break MCP setup for a run. This is the bridge
// between the plugin "mcp" contribution kind and the existing MCP catalog.
func loadPluginServers(workspace string) map[string]*ServerConfig {
	reg, err := plugin.Load()
	if err != nil {
		return map[string]*ServerConfig{}
	}
	out := map[string]*ServerConfig{}
	for _, p := range reg.Enabled() {
		exp := reg.ExpandContextFor(p.Name(), workspace)
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
