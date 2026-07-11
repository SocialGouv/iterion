package plugin

import (
	"fmt"
	"path/filepath"
	"sort"
)

// RewriterInfo is the detail-facing projection of a RewriterSpec.
type RewriterInfo struct {
	ID           string   `json:"id"`
	SandboxMount string   `json:"sandbox_mount,omitempty"`
	TimeoutMS    int      `json:"timeout_ms,omitempty"`
	Argv         []string `json:"argv,omitempty"`
}

// MCPServerInfo is the detail-facing projection of an MCPServerSpec: the
// server's name, transport, and how it is reached (command+args for stdio,
// url for http/sse).
type MCPServerInfo struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
}

// HookInfo surfaces one hook event contributed by a plugin: the claude_code
// event name (PreToolUse, Stop, …) and the raw shell command strings the hook
// fires — the studio shows these verbatim so the operator can vet what an
// opt-in plugin runs on tool events.
type HookInfo struct {
	Event    string   `json:"event"`
	Commands []string `json:"commands,omitempty"`
}

// LifecycleInfo is the detail-facing projection of a LifecycleSpec.
type LifecycleInfo struct {
	Index   string `json:"index,omitempty"`
	Refresh string `json:"refresh,omitempty"`
}

// Detail is the studio-facing full projection of one plugin: the listing View
// plus README and every contribution spelled out (what a rewriter injects,
// which MCP servers start, which files mirror, what shell the hooks fire).
type Detail struct {
	View       View            `json:"view"`
	Readme     string          `json:"readme,omitempty"`
	AutoIndex  bool            `json:"auto_index"`
	Rewriters  []RewriterInfo  `json:"rewriters,omitempty"`
	MCPServers []MCPServerInfo `json:"mcp_servers,omitempty"`
	Skills     []string        `json:"skills,omitempty"`
	Commands   []string        `json:"commands,omitempty"`
	Agents     []string        `json:"agents,omitempty"`
	Hooks      []HookInfo      `json:"hooks,omitempty"`
	Lifecycle  *LifecycleInfo  `json:"lifecycle,omitempty"`
	Dir        string          `json:"dir,omitempty"`
}

// DetailFor builds the full detail projection for one plugin. The README is
// read from the plugin's own file tree — the install dir for an installed
// plugin, the embedded FS for a builtin (builtins ship without READMEs today,
// which yields an empty string through the same lookup, not a skipped path).
func (r *Registry) DetailFor(name string) (Detail, error) {
	p, ok := r.Get(name)
	if !ok {
		return Detail{}, fmt.Errorf("plugin %q not found", name)
	}
	readme, err := readReadmeFS(p.fsys)
	if err != nil {
		return Detail{}, err
	}
	hooks, err := hookInfos(p)
	if err != nil {
		return Detail{}, err
	}
	c := p.Manifest.Contributes
	d := Detail{
		View:      r.fillConfigView(p.View(), p),
		Readme:    readme,
		AutoIndex: p.Manifest.AutoIndex,
		Skills:    baseNames(c.Skills),
		Commands:  baseNames(c.Commands),
		Agents:    baseNames(c.Agents),
		Hooks:     hooks,
		Dir:       p.Dir,
	}
	for _, rw := range c.Rewriters {
		d.Rewriters = append(d.Rewriters, RewriterInfo{
			ID:           rw.ID,
			SandboxMount: rw.SandboxMount,
			TimeoutMS:    rw.Invoke.TimeoutMs,
			Argv:         rw.Invoke.Argv,
		})
	}
	for _, s := range c.MCPServers {
		transport := s.Transport
		if transport == "" {
			transport = "stdio"
		}
		d.MCPServers = append(d.MCPServers, MCPServerInfo{
			Name:      s.Name,
			Transport: transport,
			Command:   s.Command,
			Args:      s.Args,
			URL:       s.URL,
		})
	}
	if c.Lifecycle != nil {
		d.Lifecycle = &LifecycleInfo{Index: c.Lifecycle.Index, Refresh: c.Lifecycle.Refresh}
	}
	return d, nil
}

// hookInfos flattens a plugin's hook fragments into per-event command lists.
// A fragment's hooks map is {<Event>: [{matcher, hooks: [{type, command}]}]}
// (the claude_code settings.json shape); commands for the same event across
// fragments are merged, and events are sorted for a stable projection.
func hookInfos(p *Plugin) ([]HookInfo, error) {
	fragments, err := p.HookFragments()
	if err != nil {
		return nil, err
	}
	byEvent := map[string][]string{}
	for _, hooks := range fragments {
		for event, raw := range hooks {
			entries, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("plugin %q: hooks event %q is not a list", p.Name(), event)
			}
			for _, e := range entries {
				entry, ok := e.(map[string]any)
				if !ok {
					continue
				}
				inner, ok := entry["hooks"].([]any)
				if !ok {
					continue
				}
				for _, h := range inner {
					hm, ok := h.(map[string]any)
					if !ok {
						continue
					}
					if cmd, ok := hm["command"].(string); ok && cmd != "" {
						byEvent[event] = append(byEvent[event], cmd)
					}
				}
			}
		}
	}
	events := make([]string, 0, len(byEvent))
	for ev := range byEvent {
		events = append(events, ev)
	}
	sort.Strings(events)
	out := make([]HookInfo, 0, len(events))
	for _, ev := range events {
		out = append(out, HookInfo{Event: ev, Commands: byEvent[ev]})
	}
	return out, nil
}

// baseNames maps contributed file paths to their base names (the studio lists
// the mirrored filenames, not the plugin-internal layout).
func baseNames(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}
