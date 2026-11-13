package lsp

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestRegistersAndRetrievesServer(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	rootPath := "/workspace"
	reg.Register("rust", StatusConnected, &rootPath, []string{"hover", "completion"})

	server, ok := reg.Get("rust")
	if !ok {
		t.Fatal("server should exist")
	}
	if server.Language != "rust" {
		t.Errorf("Language = %q, want rust", server.Language)
	}
	if server.Status != StatusConnected {
		t.Errorf("Status = %v, want %v", server.Status, StatusConnected)
	}
	if len(server.Capabilities) != 2 {
		t.Errorf("len(Capabilities) = %d, want 2", len(server.Capabilities))
	}
}

func TestFindsServerByFileExtension(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)
	reg.Register("typescript", StatusConnected, nil, nil)

	rsServer, ok := reg.FindServerForPath("src/main.rs")
	if !ok {
		t.Fatal("should find rust server")
	}
	if rsServer.Language != "rust" {
		t.Errorf("Language = %q, want rust", rsServer.Language)
	}

	tsServer, ok := reg.FindServerForPath("src/index.ts")
	if !ok {
		t.Fatal("should find typescript server")
	}
	if tsServer.Language != "typescript" {
		t.Errorf("Language = %q, want typescript", tsServer.Language)
	}

	_, ok = reg.FindServerForPath("data.csv")
	if ok {
		t.Error("should not find server for .csv")
	}
}

func TestManagesDiagnostics(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)

	src := "rust-analyzer"
	err := reg.AddDiagnostics("rust", []LspDiagnostic{
		{Path: "src/main.rs", Line: 10, Character: 5, Severity: "error", Message: "mismatched types", Source: &src},
	})
	if err != nil {
		t.Fatalf("AddDiagnostics() error: %v", err)
	}

	diags := reg.GetDiagnostics("src/main.rs")
	if len(diags) != 1 {
		t.Fatalf("len(diags) = %d, want 1", len(diags))
	}
	if diags[0].Message != "mismatched types" {
		t.Errorf("Message = %q, want %q", diags[0].Message, "mismatched types")
	}

	reg.ClearDiagnostics("rust")
	if diags := reg.GetDiagnostics("src/main.rs"); len(diags) != 0 {
		t.Errorf("after clear: len(diags) = %d, want 0", len(diags))
	}
}

func TestDispatchesDiagnosticsAction(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)
	reg.AddDiagnostics("rust", []LspDiagnostic{
		{Path: "src/lib.rs", Line: 1, Character: 0, Severity: "warning", Message: "unused import"},
	})

	path := "src/lib.rs"
	result, err := reg.Dispatch("diagnostics", &path, nil, nil, nil)
	if err != nil {
		t.Fatalf("Dispatch() error: %v", err)
	}

	var data map[string]any
	json.Unmarshal(result, &data)
	if data["count"].(float64) != 1 {
		t.Errorf("count = %v, want 1", data["count"])
	}
}

func TestDispatchesHoverAction(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)

	path := "src/main.rs"
	line := uint32(10)
	char := uint32(5)
	result, err := reg.Dispatch("hover", &path, &line, &char, nil)
	if err != nil {
		t.Fatalf("Dispatch() error: %v", err)
	}

	var data map[string]any
	json.Unmarshal(result, &data)
	if data["action"] != "hover" {
		t.Errorf("action = %v, want hover", data["action"])
	}
	if data["language"] != "rust" {
		t.Errorf("language = %v, want rust", data["language"])
	}
}

func TestRejectsActionOnDisconnectedServer(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusDisconnected, nil, nil)

	path := "src/main.rs"
	line := uint32(1)
	char := uint32(0)
	_, err := reg.Dispatch("hover", &path, &line, &char, nil)
	if err == nil {
		t.Fatal("expected error for disconnected server")
	}
}

func TestRejectsUnknownAction(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	path := "file.rs"
	_, err := reg.Dispatch("unknown_action", &path, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestDisconnectsServer(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)
	if reg.Len() != 1 {
		t.Errorf("Len = %d, want 1", reg.Len())
	}

	removed := reg.Disconnect("rust")
	if removed == nil {
		t.Fatal("expected removed server")
	}
	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
}

func TestLspActionFromStrAllAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  LspAction
		ok    bool
	}{
		{"diagnostics", ActionDiagnostics, true},
		{"hover", ActionHover, true},
		{"definition", ActionDefinition, true},
		{"goto_definition", ActionDefinition, true},
		{"references", ActionReferences, true},
		{"find_references", ActionReferences, true},
		{"completion", ActionCompletion, true},
		{"completions", ActionCompletion, true},
		{"symbols", ActionSymbols, true},
		{"document_symbols", ActionSymbols, true},
		{"format", ActionFormat, true},
		{"formatting", ActionFormat, true},
		{"unknown", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseAction(tc.input)
		if ok != tc.ok {
			t.Errorf("ParseAction(%q): ok = %v, want %v", tc.input, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Errorf("ParseAction(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestLspServerStatusDisplayAllVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status LspServerStatus
		want   string
	}{
		{StatusConnected, "connected"},
		{StatusDisconnected, "disconnected"},
		{StatusStarting, "starting"},
		{StatusError, "error"},
	}
	for _, tc := range cases {
		if tc.status.String() != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.status, tc.status.String(), tc.want)
		}
	}
}

func TestDispatchDiagnosticsWithoutPathAggregates(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)
	reg.Register("python", StatusConnected, nil, nil)

	src1 := "rust-analyzer"
	reg.AddDiagnostics("rust", []LspDiagnostic{
		{Path: "src/lib.rs", Line: 1, Character: 0, Severity: "warning", Message: "unused import", Source: &src1},
	})
	src2 := "pyright"
	reg.AddDiagnostics("python", []LspDiagnostic{
		{Path: "script.py", Line: 2, Character: 4, Severity: "error", Message: "undefined name", Source: &src2},
	})

	result, err := reg.Dispatch("diagnostics", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Dispatch() error: %v", err)
	}

	var data map[string]any
	json.Unmarshal(result, &data)
	if data["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2", data["count"])
	}
	diags := data["diagnostics"].([]any)
	if len(diags) != 2 {
		t.Errorf("len(diagnostics) = %d, want 2", len(diags))
	}
}

func TestDispatchNonDiagnosticsRequiresPath(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	line := uint32(1)
	char := uint32(0)
	_, err := reg.Dispatch("hover", nil, &line, &char, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "path is required for this LSP action" {
		t.Errorf("error = %q, want %q", err.Error(), "path is required for this LSP action")
	}
}

func TestDispatchNoServerForPathErrors(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	path := "notes.md"
	line := uint32(1)
	char := uint32(0)
	_, err := reg.Dispatch("hover", &path, &line, &char, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "no LSP server available for path: notes.md" {
		t.Errorf("error = %q", err.Error())
	}
}

func TestDispatchDisconnectedServerErrorPayload(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("typescript", StatusDisconnected, nil, nil)

	path := "src/index.ts"
	line := uint32(3)
	char := uint32(2)
	_, err := reg.Dispatch("hover", &path, &line, &char, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFindServerForAllExtensions(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	for _, lang := range []string{"rust", "typescript", "javascript", "python", "go", "java", "c", "cpp", "ruby", "lua"} {
		reg.Register(lang, StatusConnected, nil, nil)
	}

	cases := []struct {
		path     string
		expected string
	}{
		{"src/main.rs", "rust"},
		{"src/index.ts", "typescript"},
		{"src/view.tsx", "typescript"},
		{"src/app.js", "javascript"},
		{"src/app.jsx", "javascript"},
		{"script.py", "python"},
		{"main.go", "go"},
		{"Main.java", "java"},
		{"native.c", "c"},
		{"native.h", "c"},
		{"native.cpp", "cpp"},
		{"native.hpp", "cpp"},
		{"native.cc", "cpp"},
		{"script.rb", "ruby"},
		{"script.lua", "lua"},
	}
	for _, tc := range cases {
		server, ok := reg.FindServerForPath(tc.path)
		if !ok {
			t.Errorf("FindServerForPath(%q): not found", tc.path)
			continue
		}
		if server.Language != tc.expected {
			t.Errorf("FindServerForPath(%q) = %q, want %q", tc.path, server.Language, tc.expected)
		}
	}
}

func TestFindServerForPathNoExtension(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)

	_, ok := reg.FindServerForPath("Makefile")
	if ok {
		t.Error("should not find server for Makefile")
	}
}

func TestListServersWithMultiple(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)
	reg.Register("typescript", StatusStarting, nil, nil)
	reg.Register("python", StatusError, nil, nil)

	servers := reg.ListServers()
	if len(servers) != 3 {
		t.Errorf("len(servers) = %d, want 3", len(servers))
	}
}

func TestGetMissingServerReturnsNone(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	_, ok := reg.Get("missing")
	if ok {
		t.Error("should not find missing server")
	}
}

func TestAddDiagnosticsMissingLanguageErrors(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	err := reg.AddDiagnostics("missing", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetDiagnosticsAcrossServers(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	sharedPath := "shared/file.txt"
	reg.Register("rust", StatusConnected, nil, nil)
	reg.Register("python", StatusConnected, nil, nil)

	reg.AddDiagnostics("rust", []LspDiagnostic{
		{Path: sharedPath, Line: 4, Character: 1, Severity: "warning", Message: "warn"},
	})
	reg.AddDiagnostics("python", []LspDiagnostic{
		{Path: sharedPath, Line: 8, Character: 3, Severity: "error", Message: "err"},
	})

	diags := reg.GetDiagnostics(sharedPath)
	if len(diags) != 2 {
		t.Errorf("len(diags) = %d, want 2", len(diags))
	}
}

func TestClearDiagnosticsMissingLanguageErrors(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	err := reg.ClearDiagnostics("missing")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestConcurrentLspAccess(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Register("rust", StatusConnected, nil, nil)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.AddDiagnostics("rust", []LspDiagnostic{
				{Path: "test.rs", Line: 1, Character: 0, Severity: "warning", Message: "test"},
			})
			reg.GetDiagnostics("test.rs")
			reg.ListServers()
		}()
	}
	wg.Wait()

	diags := reg.GetDiagnostics("test.rs")
	if len(diags) != 100 {
		t.Errorf("len(diags) = %d, want 100", len(diags))
	}
}
