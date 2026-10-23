package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Focused unit coverage for parser_mcp.go (split out of parser.go in
// commit 9f67d6fd). The existing TestMCPServerDecl /
// TestWorkflowAndNodeMCPBlocks in parser_test.go cover the happy path
// for http transport; the cases below fill the remaining branches:
// stdio + sse transports, command/args propagation, error
// diagnostics, and the mcp config block knobs.

func TestMCPServer_StdioTransportWithCommandArgs(t *testing.T) {
	src := `mcp_server local:
  transport: stdio
  command: "/usr/local/bin/mcp-helper"
  args: ["--port", "9000", "--verbose"]
`
	res := parser.ParseLenient("test.bot", src)
	assertNoDiags(t, res)

	if len(res.File.MCPServers) != 1 {
		t.Fatalf("expected 1 mcp_server, got %d", len(res.File.MCPServers))
	}
	s := res.File.MCPServers[0]
	assertEq(t, "Name", s.Name, "local")
	assertEq(t, "Transport", s.Transport, ast.MCPTransportStdio)
	assertEq(t, "Command", s.Command, "/usr/local/bin/mcp-helper")
	if len(s.Args) != 3 || s.Args[0] != "--port" || s.Args[1] != "9000" || s.Args[2] != "--verbose" {
		t.Errorf("unexpected args: %v", s.Args)
	}
}

func TestMCPServer_SSETransport(t *testing.T) {
	src := `mcp_server events:
  transport: sse
  url: "https://events.example.com/mcp"
`
	res := parser.ParseLenient("test.bot", src)
	assertNoDiags(t, res)

	if len(res.File.MCPServers) != 1 {
		t.Fatalf("expected 1 mcp_server, got %d", len(res.File.MCPServers))
	}
	assertEq(t, "Transport", res.File.MCPServers[0].Transport, ast.MCPTransportSSE)
}

func TestMCPServer_InvalidTransport(t *testing.T) {
	src := `mcp_server bad:
  transport: telegraph
  url: "https://x.example.com"
`
	res := parser.ParseLenient("test.bot", src)
	if !hasDiagCode(res, parser.DiagInvalidValue) {
		t.Fatalf("expected DiagInvalidValue for unknown transport, got %v", res.Diagnostics)
	}
	// Parsing should still produce a server with MCPTransportUnknown so the
	// IR layer can decide how to handle it — verify that.
	if len(res.File.MCPServers) != 1 {
		t.Fatalf("expected 1 mcp_server even with bad transport, got %d", len(res.File.MCPServers))
	}
	assertEq(t, "Transport", res.File.MCPServers[0].Transport, ast.MCPTransportUnknown)
}

func TestMCPServer_UnknownProperty(t *testing.T) {
	src := `mcp_server bad:
  transport: http
  url: "https://x.example.com"
  bogus_prop: true
`
	res := parser.ParseLenient("test.bot", src)
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty for 'bogus_prop', got %v", res.Diagnostics)
	}
}

func TestMCPServer_MissingName(t *testing.T) {
	src := `mcp_server :
  transport: http
  url: "https://x.example.com"
`
	res := parser.ParseLenient("test.bot", src)
	if !hasDiagCode(res, parser.DiagExpectedToken) {
		t.Fatalf("expected DiagExpectedToken for missing name, got %v", res.Diagnostics)
	}
}

func TestMCPServer_AuthUnknownPropertyDoesNotLoseValidFields(t *testing.T) {
	src := `mcp_server github:
  transport: http
  url: "https://example.com/mcp"
  auth:
    type: "oauth2"
    auth_url: "https://example.com/oauth/authorize"
    bogus: "x"
    token_url: "https://example.com/oauth/token"
`
	res := parser.ParseLenient("test.bot", src)
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty for 'bogus', got %v", res.Diagnostics)
	}
	// Recovery must continue past the bad line: token_url should still parse.
	if len(res.File.MCPServers) != 1 {
		t.Fatalf("expected 1 mcp_server, got %d", len(res.File.MCPServers))
	}
	a := res.File.MCPServers[0].Auth
	if a == nil {
		t.Fatal("expected Auth block to survive partial errors")
	}
	assertEq(t, "AuthURL", a.AuthURL, "https://example.com/oauth/authorize")
	assertEq(t, "TokenURL", a.TokenURL, "https://example.com/oauth/token")
}

func TestMCPConfig_AutoloadProjectFalseAndInheritFalse(t *testing.T) {
	src := `mcp_server gh:
  transport: http
  url: "https://x.example.com"

agent worker:
  model: "anthropic/claude-sonnet-4-6"
  mcp:
    autoload_project: false
    inherit: false
    servers: [gh]
  input: in_s
  output: out_s
  system: sys
  user: usr

workflow flow:
  entry: worker
  worker -> done
`
	res := parser.ParseLenient("test.bot", src)
	assertNoDiags(t, res)
	a := res.File.Agents[0]
	if a.MCP == nil {
		t.Fatal("expected agent mcp block")
	}
	if a.MCP.AutoloadProject == nil || *a.MCP.AutoloadProject {
		t.Errorf("expected autoload_project=false, got %v", a.MCP.AutoloadProject)
	}
	if a.MCP.Inherit == nil || *a.MCP.Inherit {
		t.Errorf("expected inherit=false, got %v", a.MCP.Inherit)
	}
}

func TestMCPConfig_UnknownProperty(t *testing.T) {
	src := `mcp_server gh:
  transport: http
  url: "https://x.example.com"

agent worker:
  model: "anthropic/claude-sonnet-4-6"
  mcp:
    something_bogus: true
    servers: [gh]
  input: in_s
  output: out_s
  system: sys
  user: usr

workflow flow:
  entry: worker
  worker -> done
`
	res := parser.ParseLenient("test.bot", src)
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty inside mcp block, got %v", res.Diagnostics)
	}
	// servers: [gh] must still survive parser recovery so downstream
	// stages see a usable block.
	a := res.File.Agents[0]
	if a.MCP == nil || len(a.MCP.Servers) != 1 || a.MCP.Servers[0] != "gh" {
		t.Errorf("expected servers=[gh] preserved after recovery, got %+v", a.MCP)
	}
}
