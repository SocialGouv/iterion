package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestManager_EmptyToolListNotCached guards the cold-start poisoning fix: a
// server that lists zero tools (e.g. `npx -y <pkg>` answered tools/list before
// registering its tools) must NOT be written to the disk cache, or every later
// run on the pod gets a fast empty cache hit and the server's tools never
// appear. EnsureServers still succeeds (empty is not an error); the next run
// simply re-lists.
func TestManager_EmptyToolListNotCached(t *testing.T) {
	// A server with NO tools registered.
	mcpServer := gomcp.NewServer(&gomcp.Implementation{Name: "empty", Version: "v0"}, nil)
	handler := gomcp.NewStreamableHTTPHandler(
		func(r *http.Request) *gomcp.Server { return mcpServer },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	cfg := map[string]*ServerConfig{
		"empty": {Name: "empty", Transport: TransportHTTP, URL: server.URL},
	}
	cache := NewToolCache(t.TempDir(), time.Hour)
	m := NewManager(cfg, WithToolCache(cache))
	defer m.Close()

	if err := m.EnsureServers(context.Background(), tool.NewRegistry(), []string{"empty"}); err != nil {
		t.Fatalf("EnsureServers on a zero-tool server should not error: %v", err)
	}
	if _, ok := cache.Get("empty", cfg["empty"]); ok {
		t.Fatal("an empty tool list must NOT be cached (would poison every later run on the pod)")
	}
}
