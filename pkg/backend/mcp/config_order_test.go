package mcp

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Of several invalid servers, a refusal names the same one on every run —
// the first by name — wherever the servers come from: a project's
// .mcp.json, the workflow's own declarations, an auth that cannot be
// prepared. Thirty runs over four names: a map's order would name another
// one at some point.
func TestAnInvalidServerIsNamedTheSameOnEveryRun(t *testing.T) {
	t.Setenv(EnvAutoLoad, "true")
	projectDir := t.TempDir()
	writeProjectMCPFile(t, projectDir, `{"mcpServers": {"delta": {"type": "stdio"}, "beta": {"type": "stdio"}, "alpha": {"type": "stdio"}, "gamma": {"type": "stdio"}}}`)
	explicit := map[string]*ir.MCPServer{}
	withAuth := map[string]*ServerConfig{}
	for _, name := range []string{"delta", "beta", "alpha", "gamma"} {
		explicit[name] = &ir.MCPServer{Name: name, Transport: ir.MCPTransportStdio}
		withAuth[name] = &ServerConfig{Name: name, Transport: TransportStdio, Command: "srv", Auth: &AuthConfig{Type: "basic"}}
	}
	broker, err := NewOAuthBroker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, _, err := loadProjectServers(projectDir); err == nil || !strings.Contains(err.Error(), `"alpha"`) {
			t.Fatalf("run %d: the project's refusal names %v, want alpha", i, err)
		}
		if _, err := mergeCatalog(nil, explicit); err == nil || !strings.Contains(err.Error(), `"alpha"`) {
			t.Fatalf("run %d: the catalog's refusal names %v, want alpha", i, err)
		}
		if err := PrepareAuth(withAuth, broker); err == nil || !strings.Contains(err.Error(), `"alpha"`) {
			t.Fatalf("run %d: the auth refusal names %v, want alpha", i, err)
		}
	}
}
