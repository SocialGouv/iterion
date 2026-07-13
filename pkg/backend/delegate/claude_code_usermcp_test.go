package delegate

import (
	"bytes"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestWireUserMCP_AddsValidServersSkipsInvalid(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{
		AllowedTools: []string{"web_fetch"}, // restricted → extras get FQNs
		MCPServers: []TaskMCPServer{
			{Name: "firecrawl", Transport: "stdio", Command: "npx", Args: []string{"-y", "firecrawl-mcp"}},
			{Name: "remote", Transport: "http", URL: "http://firecrawl.internal/mcp"},
			{Name: "streamy", Transport: "sse", URL: "http://x/sse"},
			{Name: "nocmd", Transport: "stdio"}, // empty command → skipped
			{Name: "nourl", Transport: "http"},  // empty url → skipped
		},
	}
	var extras []string
	opts := b.wireUserMCP(task, nil, &extras)

	if len(opts) != 3 {
		t.Fatalf("expected 3 servers wired (stdio+http+sse), got %d", len(opts))
	}
	want := map[string]bool{"mcp__firecrawl__*": false, "mcp__remote__*": false, "mcp__streamy__*": false}
	for _, e := range extras {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for fqn, seen := range want {
		if !seen {
			t.Errorf("expected allow-list FQN %q for a restricted node, missing (extras=%v)", fqn, extras)
		}
	}
	for _, e := range extras {
		if e == "mcp__nocmd__*" || e == "mcp__nourl__*" {
			t.Errorf("skipped server leaked into extras: %q", e)
		}
	}
}

func TestWireUserMCP_SkipsReservedInternalNames(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{
		AllowedTools: []string{"web_fetch"},
		MCPServers: []TaskMCPServer{
			{Name: askUserMCPServerName, Transport: "stdio", Command: "evil"}, // reserved → skipped
			{Name: boardMCPServerName, Transport: "http", URL: "http://x/mcp"}, // reserved → skipped
			{Name: "ok", Transport: "stdio", Command: "npx"},                   // wired
		},
	}
	var extras []string
	opts := b.wireUserMCP(task, nil, &extras)
	if len(opts) != 1 {
		t.Fatalf("expected only the non-reserved server wired, got %d", len(opts))
	}
	for _, e := range extras {
		if e == "mcp__"+askUserMCPServerName+"__*" || e == "mcp__"+boardMCPServerName+"__*" {
			t.Errorf("reserved server leaked into extras: %q", e)
		}
	}
}

func TestWireUserMCP_NoExtrasWhenToolsetUnrestricted(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	task := Task{
		// empty AllowedTools = "no restriction" → native toolset stays, and
		// we must NOT allow-list (which would imply a restriction).
		MCPServers: []TaskMCPServer{
			{Name: "firecrawl", Transport: "stdio", Command: "npx"},
		},
	}
	var extras []string
	opts := b.wireUserMCP(task, nil, &extras)
	if len(opts) != 1 {
		t.Fatalf("expected 1 server wired, got %d", len(opts))
	}
	if len(extras) != 0 {
		t.Errorf("expected no allow-list additions on an unrestricted node, got %v", extras)
	}
}
