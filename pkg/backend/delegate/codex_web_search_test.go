package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func TestCodexWebSearchOptionHonorsDSLTool(t *testing.T) {
	tests := []struct {
		name  string
		tools []string
		want  string
	}{
		{name: "undeclared is disabled", want: codexWebSearchModeDisabled},
		{name: "other tools do not expose search", tools: []string{"bash", "grep"}, want: codexWebSearchModeDisabled},
		{name: "canonical DSL tool enables live search", tools: []string{"web_search"}, want: codexWebSearchModeLive},
		{name: "portable TitleCase alias enables live search", tools: []string{"WebSearch"}, want: codexWebSearchModeLive},
		{name: "hyphenated alias enables live search", tools: []string{"web-search"}, want: codexWebSearchModeLive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &codexsdk.CodexAgentOptions{}
			codexWebSearchOption(Task{AllowedTools: tt.tools})(opts)
			if got := opts.Config["web_search"]; got != tt.want {
				t.Fatalf("web_search config = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateCodexWebSearchCapabilityRejectsOldCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	command := writeCodexVersionFixture(t, "codex-cli 0.102.0")
	_, err := validateCodexWebSearchCapability(context.Background(), command)
	if err == nil {
		t.Fatal("expected an incompatible CLI error")
	}
	for _, fragment := range []string{"web_search mode unavailable", "0.102.0", codexWebSearchMinCLIVersion, "upgrade Codex CLI"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}

func TestCodexDisabledWebSearchStillValidatesModeCapability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	command := writeCodexVersionFixture(t, "codex-cli 0.102.0")
	result, err := (&CodexBackend{Command: command, Logger: testLogger()}).Execute(context.Background(), Task{})
	if err == nil || !strings.Contains(err.Error(), "web_search mode unavailable") {
		t.Fatalf("disabled web_search path did not reject incompatible CLI: result=%+v err=%v", result, err)
	}
	if result.ExitCode != -1 || result.BackendName != BackendCodex {
		t.Fatalf("result = %+v", result)
	}
}

func TestValidateCodexWebSearchCapabilityAcceptsPinnedMinimum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	command := writeCodexVersionFixture(t, "codex-cli "+codexWebSearchMinCLIVersion)
	got, err := validateCodexWebSearchCapability(context.Background(), command)
	if err != nil {
		t.Fatalf("minimum compatible CLI rejected: %v", err)
	}
	if got != command {
		t.Fatalf("resolved CLI = %q, want probed path %q", got, command)
	}
}

func TestValidateCodexWebSearchCapabilityIgnoresStderrVersions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	command := writeCodexVersionFixtureScript(t,
		"printf '%s\\n' 'npm notice 99.99.99' >&2\nprintf '%s\\n' 'codex-cli "+codexWebSearchMinCLIVersion+"'\n")
	if _, err := validateCodexWebSearchCapability(context.Background(), command); err != nil {
		t.Fatalf("stderr noise replaced the Codex stdout version: %v", err)
	}
}

func TestValidateCodexWebSearchCapabilityHonorsSDKSkipAndPinsPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	command := writeCodexVersionFixture(t, "not-a-semver")
	t.Setenv("CODEX_CLI_SKIP_VERSION_CHECK", "1")
	got, err := validateCodexWebSearchCapability(context.Background(), command)
	if err != nil {
		t.Fatalf("SDK version-check escape hatch rejected: %v", err)
	}
	if got != command {
		t.Fatalf("resolved CLI = %q, want %q", got, command)
	}
}

func TestEmitCodexToolHooksSurfacesWebSearchLifecycle(t *testing.T) {
	type startedCall struct {
		name, id string
		input    json.RawMessage
	}
	type completedCall struct {
		name, id, output string
		isError          bool
	}
	var started []startedCall
	var completed []completedCall
	hooks := TaskHooks{
		OnToolStarted: func(name, id string, input json.RawMessage) {
			started = append(started, startedCall{name: name, id: id, input: input})
		},
		OnToolCalled: func(name, id string, isError bool, output string) {
			completed = append(completed, completedCall{name: name, id: id, isError: isError, output: output})
		},
	}
	inFlight := map[string]string{}

	startAudit, err := codexsdk.NewAuditEnvelope("item.started", "", map[string]any{
		"item": map[string]any{"type": "webSearch", "id": "search-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexToolHooks(hooks, &codexsdk.AssistantMessage{
		Audit: startAudit,
		Content: []codexsdk.ContentBlock{&codexsdk.ToolUseBlock{
			ID: "search-1", Name: "WebSearch", Input: map[string]any{"query": "latest Iterion release"},
		}},
	}, inFlight)

	completeAudit, err := codexsdk.NewAuditEnvelope("item.completed", "", map[string]any{
		"item": map[string]any{
			"type":    "webSearch",
			"id":      "search-1",
			"query":   "latest Iterion release",
			"action":  map[string]any{"type": "search", "query": "latest Iterion release"},
			"results": []any{map[string]any{"url": "https://github.com/SocialGouv/iterion/releases"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexToolHooks(hooks, &codexsdk.AssistantMessage{
		Audit: completeAudit,
		Content: []codexsdk.ContentBlock{&codexsdk.ToolUseBlock{
			ID: "search-1", Name: "WebSearch", Input: map[string]any{"query": "latest Iterion release"},
		}},
	}, inFlight)

	if len(started) != 1 || started[0].name != "WebSearch" || started[0].id != "search-1" {
		t.Fatalf("started = %+v", started)
	}
	if len(completed) != 1 || completed[0].name != "WebSearch" || completed[0].id != "search-1" || completed[0].isError {
		t.Fatalf("completed = %+v", completed)
	}
	if !strings.Contains(completed[0].output, "https://github.com/SocialGouv/iterion/releases") {
		t.Fatalf("completed output lost Web search source: %s", completed[0].output)
	}
	if len(inFlight) != 0 {
		t.Fatalf("in-flight search was not cleared: %v", inFlight)
	}
}

func TestEmitCodexToolHooksKeepsShellDistinctFromWebSearch(t *testing.T) {
	var names []string
	hooks := TaskHooks{OnToolStarted: func(name, _ string, _ json.RawMessage) {
		names = append(names, name)
	}}
	audit, err := codexsdk.NewAuditEnvelope("item.started", "", map[string]any{"item": map[string]any{"type": "commandExecution"}})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexToolHooks(hooks, &codexsdk.AssistantMessage{
		Audit: audit,
		Content: []codexsdk.ContentBlock{
			&codexsdk.ToolUseBlock{ID: "shell-1", Name: "Bash", Input: map[string]any{"command": "pwd"}},
			&codexsdk.ToolUseBlock{ID: "search-1", Name: "WebSearch", Input: map[string]any{"query": "Iterion"}},
		},
	}, map[string]string{})
	if strings.Join(names, ",") != "Bash,WebSearch" {
		t.Fatalf("tool names = %v, want distinct Bash and WebSearch", names)
	}
}

func TestEmitCodexToolHooksIgnoresItemUpdatedForStartLifecycle(t *testing.T) {
	var starts int
	hooks := TaskHooks{OnToolStarted: func(string, string, json.RawMessage) { starts++ }}
	inFlight := map[string]string{"search-1": "WebSearch"}
	audit, err := codexsdk.NewAuditEnvelope("item.updated", "", map[string]any{
		"item": map[string]any{"type": "webSearch", "id": "search-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexToolHooks(hooks, &codexsdk.AssistantMessage{
		Audit: audit,
		Content: []codexsdk.ContentBlock{&codexsdk.ToolUseBlock{
			ID: "search-1", Name: "WebSearch", Input: map[string]any{"query": "Iterion"},
		}},
	}, inFlight)
	if starts != 0 {
		t.Fatalf("item.updated emitted %d duplicate starts", starts)
	}
}

func TestEmitCodexToolHooksDerivesResultlessItemFailure(t *testing.T) {
	var gotError bool
	hooks := TaskHooks{OnToolCalled: func(_ string, _ string, isError bool, _ string) {
		gotError = isError
	}}
	audit, err := codexsdk.NewAuditEnvelope("item.completed", "", map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"item": map[string]any{
				"type": "fileChange", "id": "edit-1", "status": "failed",
				"error": map[string]any{"message": "read-only file system"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	emitCodexToolHooks(hooks, &codexsdk.AssistantMessage{
		Audit: audit,
		Content: []codexsdk.ContentBlock{&codexsdk.ToolUseBlock{
			ID: "edit-1", Name: "Edit", Input: map[string]any{"file_path": "README.md"},
		}},
	}, map[string]string{"edit-1": "Edit"})
	if !gotError {
		t.Fatal("failed result-less item was emitted as successful")
	}
}

func writeCodexVersionFixture(t *testing.T, output string) string {
	t.Helper()
	return writeCodexVersionFixtureScript(t, "printf '%s\\n' '"+output+"'\n")
}

func writeCodexVersionFixtureScript(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	body := "#!/bin/sh\n" + script
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
