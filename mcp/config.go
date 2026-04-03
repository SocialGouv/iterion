package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/ir"
)

const (
	// EnvAutoLoad controls automatic project .mcp.json loading.
	EnvAutoLoad = "ITERION_MCP_AUTOLOAD"
)

// Transport identifies the transport used by an MCP server.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
	TransportSSE   Transport = "sse"
)

// ServerConfig is the runtime MCP server definition used by the manager.
type ServerConfig struct {
	Name      string
	Transport Transport
	Command   string
	Args      []string
	URL       string
	Headers   map[string]string
	WorkDir   string            // working directory for stdio server processes
	Env       map[string]string // extra environment variables for stdio server processes
}

// PrepareWorkflow resolves the final MCP catalog and active server sets for a
// compiled workflow. It merges project .mcp.json, top-level `mcp_server`
// declarations, and built-in presets, then applies workflow/node filters.
func PrepareWorkflow(wf *ir.Workflow, projectDir string) error {
	if wf == nil {
		return nil
	}

	projectServers, projectNames, err := loadProjectServers(projectDir)
	if err != nil {
		return err
	}

	catalog, err := mergeCatalog(projectServers, wf.MCPServers)
	if err != nil {
		return err
	}

	workflowActive, err := resolveWorkflowActiveServers(wf.MCP, projectNames, catalog)
	if err != nil {
		return err
	}

	wf.ActiveMCPServers = workflowActive
	wf.ResolvedMCPServers = make(map[string]*ir.MCPServer, len(catalog))
	for name, cfg := range catalog {
		wf.ResolvedMCPServers[name] = &ir.MCPServer{
			Name:      cfg.Name,
			Transport: toIRTransport(cfg.Transport),
			Command:   cfg.Command,
			Args:      append([]string(nil), cfg.Args...),
			URL:       cfg.URL,
			Headers:   cloneStringMap(cfg.Headers),
		}
	}

	for _, node := range wf.Nodes {
		active, err := resolveNodeActiveServers(node.MCP, workflowActive, catalog)
		if err != nil {
			return fmt.Errorf("mcp: node %q: %w", node.ID, err)
		}
		node.ActiveMCPServers = active
	}

	return nil
}

func AutoLoadProjectEnabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(EnvAutoLoad)))
	return value != "0" && value != "false"
}

func NativePresets() map[string]*ServerConfig {
	return map[string]*ServerConfig{
		"claude_code": {
			Name:      "claude_code",
			Transport: TransportStdio,
			Command:   "claude",
			Args:      []string{"mcp", "serve"},
		},
		"codex": {
			Name:      "codex",
			Transport: TransportStdio,
			Command:   "codex",
			Args: []string{
				"mcp-server",
				"-c", `sandbox="danger-full-access"`,
			},
		},
	}
}

func loadProjectServers(projectDir string) (map[string]*ServerConfig, []string, error) {
	if !AutoLoadProjectEnabled() {
		return map[string]*ServerConfig{}, nil, nil
	}

	path := filepath.Join(projectDir, ".mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*ServerConfig{}, nil, nil
		}
		return nil, nil, fmt.Errorf("mcp: read %s: %w", path, err)
	}

	var file struct {
		MCPServers map[string]struct {
			Type      string            `json:"type"`
			Transport string            `json:"transport"`
			Command   string            `json:"command"`
			Args      []string          `json:"args"`
			URL       string            `json:"url"`
			Headers   map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("mcp: parse %s: %w", path, err)
	}

	names := make([]string, 0, len(file.MCPServers))
	servers := make(map[string]*ServerConfig, len(file.MCPServers))
	for name, raw := range file.MCPServers {
		cfg := &ServerConfig{
			Name:      name,
			Transport: normalizeTransport(raw.Type, raw.Transport, raw.Command, raw.URL),
			Command:   raw.Command,
			Args:      append([]string(nil), raw.Args...),
			URL:       raw.URL,
			Headers:   cloneStringMap(raw.Headers),
		}
		if err := validateServerConfig(cfg); err != nil {
			return nil, nil, fmt.Errorf("mcp: project server %q: %w", name, err)
		}
		names = append(names, name)
		servers[name] = cfg
	}
	sort.Strings(names)
	return servers, names, nil
}

func mergeCatalog(project map[string]*ServerConfig, explicit map[string]*ir.MCPServer) (map[string]*ServerConfig, error) {
	catalog := make(map[string]*ServerConfig, len(project)+len(explicit)+2)
	for name, cfg := range project {
		catalog[name] = cloneServerConfig(cfg)
	}
	for name, cfg := range explicit {
		catalog[name] = &ServerConfig{
			Name:      cfg.Name,
			Transport: fromIRTransport(cfg.Transport),
			Command:   cfg.Command,
			Args:      append([]string(nil), cfg.Args...),
			URL:       cfg.URL,
			Headers:   cloneStringMap(cfg.Headers),
		}
	}
	for name, cfg := range NativePresets() {
		if _, exists := catalog[name]; exists {
			continue
		}
		catalog[name] = cloneServerConfig(cfg)
	}

	for name, cfg := range catalog {
		if err := validateServerConfig(cfg); err != nil {
			return nil, fmt.Errorf("mcp: server %q: %w", name, err)
		}
	}
	return catalog, nil
}

func resolveWorkflowActiveServers(cfg *ir.MCPConfig, projectNames []string, catalog map[string]*ServerConfig) ([]string, error) {
	autoload := true
	if cfg != nil && cfg.AutoloadProject != nil {
		autoload = *cfg.AutoloadProject
	}

	set := newOrderedSet()
	if autoload {
		for _, name := range projectNames {
			set.Add(name)
		}
	}

	if cfg != nil {
		for _, name := range cfg.Servers {
			if _, ok := catalog[name]; !ok {
				return nil, fmt.Errorf("unknown MCP server %q", name)
			}
			set.Add(name)
		}
		for _, name := range cfg.Disable {
			if _, ok := catalog[name]; !ok {
				return nil, fmt.Errorf("unknown MCP server %q", name)
			}
			set.Remove(name)
		}
	}

	return set.Values(), nil
}

func resolveNodeActiveServers(cfg *ir.MCPConfig, workflowActive []string, catalog map[string]*ServerConfig) ([]string, error) {
	inherit := true
	if cfg != nil && cfg.Inherit != nil {
		inherit = *cfg.Inherit
	}

	set := newOrderedSet()
	if inherit {
		for _, name := range workflowActive {
			set.Add(name)
		}
	}
	if cfg != nil {
		for _, name := range cfg.Servers {
			if _, ok := catalog[name]; !ok {
				return nil, fmt.Errorf("unknown MCP server %q", name)
			}
			set.Add(name)
		}
		for _, name := range cfg.Disable {
			if _, ok := catalog[name]; !ok {
				return nil, fmt.Errorf("unknown MCP server %q", name)
			}
			set.Remove(name)
		}
	}
	return set.Values(), nil
}

func validateServerConfig(cfg *ServerConfig) error {
	switch cfg.Transport {
	case TransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return fmt.Errorf("transport stdio requires command")
		}
		if strings.TrimSpace(cfg.URL) != "" {
			return fmt.Errorf("transport stdio cannot set url")
		}
	case TransportHTTP:
		if strings.TrimSpace(cfg.URL) == "" {
			return fmt.Errorf("transport http requires url")
		}
		if strings.TrimSpace(cfg.Command) != "" {
			return fmt.Errorf("transport http cannot set command")
		}
		if len(cfg.Args) > 0 {
			return fmt.Errorf("transport http cannot set args")
		}
	case TransportSSE:
		return fmt.Errorf("transport sse is not supported in v1")
	default:
		return fmt.Errorf("unsupported transport %q", cfg.Transport)
	}
	return nil
}

func normalizeTransport(typeValue, transportValue, command, url string) Transport {
	value := strings.TrimSpace(strings.ToLower(typeValue))
	if value == "" {
		value = strings.TrimSpace(strings.ToLower(transportValue))
	}
	if value == "" {
		switch {
		case strings.TrimSpace(url) != "":
			value = "http"
		case strings.TrimSpace(command) != "":
			value = "stdio"
		}
	}
	switch value {
	case "stdio":
		return TransportStdio
	case "http", "streamable-http":
		return TransportHTTP
	case "sse":
		return TransportSSE
	default:
		return ""
	}
}

func fromIRTransport(t ir.MCPTransport) Transport {
	switch t {
	case ir.MCPTransportStdio:
		return TransportStdio
	case ir.MCPTransportHTTP:
		return TransportHTTP
	case ir.MCPTransportSSE:
		return TransportSSE
	default:
		return ""
	}
}

func toIRTransport(t Transport) ir.MCPTransport {
	switch t {
	case TransportStdio:
		return ir.MCPTransportStdio
	case TransportHTTP:
		return ir.MCPTransportHTTP
	case TransportSSE:
		return ir.MCPTransportSSE
	default:
		return ir.MCPTransportUnknown
	}
}

func cloneServerConfig(cfg *ServerConfig) *ServerConfig {
	if cfg == nil {
		return nil
	}
	return &ServerConfig{
		Name:      cfg.Name,
		Transport: cfg.Transport,
		Command:   cfg.Command,
		Args:      append([]string(nil), cfg.Args...),
		URL:       cfg.URL,
		Headers:   cloneStringMap(cfg.Headers),
		WorkDir:   cfg.WorkDir,
		Env:       cloneStringMap(cfg.Env),
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

type orderedSet struct {
	index map[string]int
	items []string
}

func newOrderedSet() *orderedSet {
	return &orderedSet{index: make(map[string]int)}
}

func (s *orderedSet) Add(value string) {
	if _, exists := s.index[value]; exists {
		return
	}
	s.index[value] = len(s.items)
	s.items = append(s.items, value)
}

func (s *orderedSet) Remove(value string) {
	idx, exists := s.index[value]
	if !exists {
		return
	}
	delete(s.index, value)
	s.items = append(s.items[:idx], s.items[idx+1:]...)
	for i := idx; i < len(s.items); i++ {
		s.index[s.items[i]] = i
	}
}

func (s *orderedSet) Values() []string {
	return append([]string(nil), s.items...)
}
