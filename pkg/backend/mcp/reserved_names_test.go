package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestPrepareWorkflowRefusesInternalMCPNames(t *testing.T) {
	t.Setenv(EnvAutoLoad, "true")
	for _, name := range []string{"iterion", "iterion_board", "iterion_runs", "Iterion.Anything", "iterionish", "__iterion", ".iterion", "", "__", "."} {
		for _, source := range []string{"project", "explicit-key", "explicit-name"} {
			t.Run(source+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				wf := &ir.Workflow{ActiveMCPServers: []string{"unchanged"}}
				if source == "project" {
					data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{name: map[string]string{"type": "stdio", "command": "untrusted"}}})
					if err != nil {
						t.Fatal(err)
					}
					writeProjectMCPFile(t, dir, string(data))
				} else {
					key, cfgName := name, "ordinary"
					if source == "explicit-name" {
						key, cfgName = "ordinary", name
					}
					wf.MCPServers = map[string]*ir.MCPServer{key: {Name: cfgName, Transport: ir.MCPTransportStdio, Command: "untrusted"}}
				}
				err := PrepareWorkflow(wf, dir)
				if err == nil || !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), "rename") {
					t.Fatalf("want actionable reserved-name refusal, got %v", err)
				}
				if len(wf.ActiveMCPServers) != 1 || wf.ActiveMCPServers[0] != "unchanged" || wf.ResolvedMCPServers != nil {
					t.Fatal("refusal published a partial workflow catalog")
				}
			})
		}
	}
}

func TestMergeCatalogRefusesReservedPluginShape(t *testing.T) {
	for _, name := range []string{"iterion_board", "iterion_runs", "__iterion"} {
		_, err := mergeCatalog(map[string]*ServerConfig{name: {Name: name, Transport: TransportHTTP, URL: "https://untrusted.invalid/mcp"}}, nil)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("name %q: %v", name, err)
		}
	}
}

func TestPrepareWorkflowPreservesOrdinaryMCPNames(t *testing.T) {
	t.Setenv(EnvAutoLoad, "true")
	dir := t.TempDir()
	writeProjectMCPFile(t, dir, `{"mcpServers":{"docs":{"type":"stdio","command":"docs"},"my_iterion":{"type":"sse","url":"https://docs.invalid/sse"},"xiterion":{"type":"http","url":"https://docs.invalid/mcp"}}}`)
	wf := &ir.Workflow{MCPServers: map[string]*ir.MCPServer{"docs": {Name: "docs", Transport: ir.MCPTransportHTTP, URL: "https://override.invalid/mcp"}}}
	if err := PrepareWorkflow(wf, dir); err != nil {
		t.Fatal(err)
	}
	if len(wf.ActiveMCPServers) != 3 || wf.ResolvedMCPServers["docs"].URL != "https://override.invalid/mcp" {
		t.Fatal("ordinary servers or override lost")
	}
	writeProjectMCPFile(t, dir, `{"mcpServers":{"iterion_board":{"type":"stdio","command":"untrusted"}}}`)
	t.Setenv(EnvAutoLoad, "false")
	if err := PrepareWorkflow(&ir.Workflow{}, dir); err != nil {
		t.Fatalf("disabled project autoload must not read project names: %v", err)
	}
}
