package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// memTest sets up an isolated ITERION_HOME and returns the FS store +
// the legacy bot SpaceRef the tools run against, plus a direct Scope
// pointing at the same on-disk location for read/write assertions.
func memTest(t *testing.T, scope string) (*memory.FSStore, knowledge.SpaceRef, *memory.Scope) {
	t.Helper()
	t.Setenv("ITERION_HOME", t.TempDir())
	s, err := memory.OpenScope("/tmp/wn", scope)
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	return memory.DefaultFSStore(), memory.LegacyBotRef("/tmp/wn", scope), s
}

func TestMemoryWriteTool_RoundtripsToScope(t *testing.T) {
	store, ref, s := memTest(t, "session-continuity")
	tool := memoryWriteTool(store, ref, "")
	if tool.Name != MemoryWriteToolName {
		t.Fatalf("name: %q", tool.Name)
	}
	in, _ := json.Marshal(map[string]string{"path": "CONTEXT_BRIEF.md", "content": "# foo"})
	out, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "checkpoint written") {
		t.Fatalf("result: %q", out)
	}
	data, err := s.Read("CONTEXT_BRIEF.md")
	if err != nil || string(data) != "# foo" {
		t.Fatalf("readback: %q err=%v", string(data), err)
	}
}

func TestMemoryReadTool_ReturnsContent(t *testing.T) {
	store, ref, s := memTest(t, "session-continuity")
	_ = s.Write("brief.md", []byte("hello"))
	tool := memoryReadTool(store, ref)
	in, _ := json.Marshal(map[string]string{"path": "brief.md"})
	out, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "hello" {
		t.Fatalf("got %q", out)
	}
}

func TestMemoryListTool_EmptyAndPopulated(t *testing.T) {
	store, ref, s := memTest(t, "session-continuity")
	tool := memoryListTool(store, ref)
	out, err := tool.Execute(context.Background(), nil)
	if err != nil || out != "(empty)" {
		t.Fatalf("empty: out=%q err=%v", out, err)
	}
	_ = s.Write("a.md", []byte("x"))
	out, err = tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "a.md") {
		t.Fatalf("populated: %q", out)
	}
}

func TestMemoryPreCompactInjector_KeepsTheFirstTurn(t *testing.T) {
	store, ref, s := memTest(t, "session-continuity")
	_ = s.Write("CONTEXT_BRIEF.md", []byte("brief body"))

	inject := memoryPreCompactInjector(context.Background(), store, ref, []string{"CONTEXT_BRIEF.md"})
	original := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "orig"}}}}
	got := inject(original)
	if len(got) != 2 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Content[0].Text != "orig" || original[0].Content[0].Text != "orig" {
		t.Fatal("injection moved or mutated the original first turn")
	}
	if !got[1].IsInjected || got[1].Role != "user" {
		t.Fatalf("injected msg shape: %+v", got[1])
	}
	if !strings.Contains(got[1].Content[0].Text, "brief body") {
		t.Fatalf("missing content: %q", got[1].Content[0].Text)
	}
	if !strings.Contains(got[1].Content[0].Text, "<workspace_memory") {
		t.Fatalf("missing tag wrapper: %q", got[1].Content[0].Text)
	}
}

func TestMemoryPreCompactInjector_MissingScopeNoOp(t *testing.T) {
	store, ref, _ := memTest(t, "session-continuity")
	inject := memoryPreCompactInjector(context.Background(), store, ref, nil) // no autoload, empty scope
	got := inject([]api.Message{{Role: "user"}})
	if got != nil {
		t.Fatalf("expected nil (no-op), got %d msgs", len(got))
	}
}

func TestInstallWorkspaceMemory_WiresEverything(t *testing.T) {
	store, ref, _ := memTest(t, "session-continuity")
	opts := &GenerationOptions{}
	spec := &delegate.MemorySpec{
		Scope:            "session-continuity",
		Autoload:         nil,
		Read:             true,
		Write:            true,
		PreCompactInject: true,
	}
	if err := installWorkspaceMemory(context.Background(), opts, store, ref, spec); err != nil {
		t.Fatalf("install: %v", err)
	}
	names := make(map[string]bool)
	for _, tool := range opts.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{MemoryReadToolName, MemoryWriteToolName, MemoryListToolName} {
		if !names[want] {
			t.Fatalf("missing tool %q (got %v)", want, names)
		}
	}
	if opts.OnBeforeCompact == nil {
		t.Fatalf("PreCompactInject=true did not install OnBeforeCompact")
	}
}

func TestInstallWorkspaceMemory_EmitsAutoIndex(t *testing.T) {
	store, ref, scope := memTest(t, "whats-next")
	_ = scope.Write("brief.md", []byte("---\ntitle: Brief\ntags: [a, b]\n---\n"))
	_ = scope.Write("decisions/dropped-x.md", []byte("# Dropped X\n"))

	opts := &GenerationOptions{}
	err := installWorkspaceMemory(context.Background(), opts, store, ref, &delegate.MemorySpec{
		Scope: "whats-next",
		Read:  true,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(opts.SystemBlocks) != 1 {
		t.Fatalf("expected 1 system block (auto-index, no autoload), got %d", len(opts.SystemBlocks))
	}
	got := opts.SystemBlocks[0].Text
	for _, want := range []string{"workspace_memory_index", "brief.md", "title=\"Brief\"", "tags=\"a,b\"", "decisions/dropped-x.md", "title=\"Dropped X\""} {
		if !strings.Contains(got, want) {
			t.Fatalf("auto-index missing %q\nfull:\n%s", want, got)
		}
	}
	if strings.Contains(got, "workspace_memory scope_root") {
		t.Fatalf("autoload block leaked into index-only run:\n%s", got)
	}
}

func TestInstallWorkspaceMemory_AutoIndexPlusAutoload(t *testing.T) {
	store, ref, scope := memTest(t, "whats-next")
	_ = scope.Write("CONTEXT_BRIEF.md", []byte("---\ntitle: Brief\n---\nFULL CONTENT\n"))
	_ = scope.Write("decisions/dropped-x.md", []byte("# Dropped X\n"))

	opts := &GenerationOptions{}
	err := installWorkspaceMemory(context.Background(), opts, store, ref, &delegate.MemorySpec{
		Scope:    "whats-next",
		Autoload: []string{"CONTEXT_BRIEF.md"},
		Read:     true,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(opts.SystemBlocks) != 2 {
		t.Fatalf("expected 2 system blocks (index + autoload), got %d", len(opts.SystemBlocks))
	}
	if !strings.Contains(opts.SystemBlocks[0].Text, "workspace_memory_index") {
		t.Fatalf("first block should be the index, got:\n%s", opts.SystemBlocks[0].Text)
	}
	if !strings.Contains(opts.SystemBlocks[1].Text, "FULL CONTENT") {
		t.Fatalf("second block should carry autoloaded content, got:\n%s", opts.SystemBlocks[1].Text)
	}
}

func TestInstallWorkspaceMemory_ReadOnly(t *testing.T) {
	store, ref, _ := memTest(t, "session-continuity")
	opts := &GenerationOptions{}
	spec := &delegate.MemorySpec{
		Scope: "session-continuity",
		Read:  true,
		Write: false,
	}
	if err := installWorkspaceMemory(context.Background(), opts, store, ref, spec); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, tool := range opts.Tools {
		if tool.Name == MemoryWriteToolName {
			t.Fatalf("write tool leaked under Read=true, Write=false")
		}
	}
}

func TestInstallWorkspaceMemory_RejectsBadScope(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	opts := &GenerationOptions{}
	ref := memory.LegacyBotRef("/tmp/wn", "../escape")
	if err := installWorkspaceMemory(context.Background(), opts, memory.DefaultFSStore(), ref, &delegate.MemorySpec{Scope: "../escape"}); err == nil {
		t.Fatalf("expected scope rejection")
	}
}

// findTool returns the registered GenerationTool by name, failing the
// test if absent.
func findTool(t *testing.T, tools []GenerationTool, name string) GenerationTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not registered (have %d tools)", name, len(tools))
	return GenerationTool{}
}

// botVisionRef resolves the SpaceRef exactly as claw_backend.go does for a
// `memory: { visibility: "bot" }` block — project key from the repo root,
// bot id from the workflow name. Centralised so the bot-path tests share
// one resolution and drift with the production path, not a hand-built ref.
func botVisionRef(repo, botID string) knowledge.SpaceRef {
	return memory.ResolveSpaceRef(knowledge.VisibilityBot, "vision", botID, "", memory.SpaceRefInputs{
		ProjectID: memory.ProjectKey(repo),
		BotID:     botID,
	})
}

// TestInstallWorkspaceMemory_VisibilityBot_HonoursBotPath is the end-to-end
// proof of the per-bot cross-session memory feature Evoly relies on: a
// visibility:bot write lands at the per-bot path (NOT the legacy
// project-shared path), and a fresh session autoloads it back into the
// system prompt. This is the single test that exercises the real
// installWorkspaceMemory + FSStore + ResolveSpaceRef chain for the
// bot-visibility axis, which had no shipped consumer before Evoly.
func TestInstallWorkspaceMemory_VisibilityBot_HonoursBotPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	store := memory.DefaultFSStore()
	const repo = "/tmp/evolve-repo"
	ref := botVisionRef(repo, "evolve")

	// Session 1: write VISION.md through the registered tool.
	opts := &GenerationOptions{}
	if err := installWorkspaceMemory(context.Background(), opts, store, ref, &delegate.MemorySpec{
		Scope: "vision", Visibility: "bot", BotID: "evolve", Write: true,
	}); err != nil {
		t.Fatalf("install s1: %v", err)
	}
	writeTool := findTool(t, opts.Tools, MemoryWriteToolName)
	in, _ := json.Marshal(map[string]string{"path": "VISION.md", "content": "long-term resilience"})
	if _, err := writeTool.Execute(context.Background(), in); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The file MUST land at the per-bot path, NOT the legacy project path.
	pkey := memory.ProjectKey(repo)
	botPath := filepath.Join(home, "projects", pkey, "bots", "evolve", "memory", "vision", "VISION.md")
	if _, err := os.Stat(botPath); err != nil {
		t.Fatalf("expected per-bot file at %s: %v", botPath, err)
	}
	legacyPath := filepath.Join(home, "projects", pkey, "memory", "vision", "VISION.md")
	if _, err := os.Stat(legacyPath); err == nil {
		t.Fatalf("write leaked to legacy project path %s — visibility:bot not honoured", legacyPath)
	}

	// Session 2: a fresh install with autoload must re-inject session 1's
	// content into the system prompt (cross-session memory).
	opts2 := &GenerationOptions{}
	if err := installWorkspaceMemory(context.Background(), opts2, store, ref, &delegate.MemorySpec{
		Scope: "vision", Visibility: "bot", BotID: "evolve", Autoload: []string{"VISION.md"}, Read: true,
	}); err != nil {
		t.Fatalf("install s2: %v", err)
	}
	var autoloaded bool
	for _, b := range opts2.SystemBlocks {
		if strings.Contains(b.Text, "long-term resilience") {
			autoloaded = true
		}
	}
	if !autoloaded {
		t.Fatalf("session 2 did not autoload session 1's VISION.md; blocks=%+v", opts2.SystemBlocks)
	}
}

// TestInstallWorkspaceMemory_RejectsBadDocPath guards the agent-chosen
// document path against traversal on the bot-visibility tool path.
func TestInstallWorkspaceMemory_RejectsBadDocPath(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	store := memory.DefaultFSStore()
	ref := botVisionRef("/tmp/evolve-repo", "evolve")
	opts := &GenerationOptions{}
	if err := installWorkspaceMemory(context.Background(), opts, store, ref, &delegate.MemorySpec{
		Scope: "vision", Visibility: "bot", BotID: "evolve", Write: true,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	writeTool := findTool(t, opts.Tools, MemoryWriteToolName)
	in, _ := json.Marshal(map[string]string{"path": "../escape.md", "content": "x"})
	if _, err := writeTool.Execute(context.Background(), in); err == nil {
		t.Fatalf("expected rejection of traversal doc path")
	}
}

// TestMemoryAttribution covers the bot-id attribution stamp threaded into
// agent memory writes (blank before this; the REST path stamps the
// operator, the tool path now stamps the bot).
func TestMemoryAttribution(t *testing.T) {
	if got := memoryAttribution("evolve"); got != "bot:evolve" {
		t.Fatalf("attribution: got %q", got)
	}
	if got := memoryAttribution(""); got != "" {
		t.Fatalf("empty bot id should yield empty attribution, got %q", got)
	}
}

// Drive the real between-iterations hook and compact again with a continuation
// produced by claw itself. The old injector makes that summary an ordinary
// user turn; its preamble crowds out the decision during re-summarisation.
func TestMemoryPreCompactInjector_PreservesPreviousSummary(t *testing.T) {
	for _, role := range []string{"user", "system"} {
		t.Run(role, func(t *testing.T) {
			store, ref, scope := memTest(t, "summary-retention")
			if err := scope.Write("BRIEF.md", []byte("fresh memory")); err != nil {
				t.Fatal(err)
			}
			msg := func(text string) api.Message {
				return api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: text}}}
			}
			seed := clawrt.CompactMessages([]api.Message{msg("UNIQUE_EARLIER_DECISION must survive"), msg("first recent")}, clawrt.CompactionConfig{PreserveRecentMessages: 1, MaxEstimatedTokens: 1})
			if seed == nil || !strings.Contains(seed.Summary, "UNIQUE_EARLIER_DECISION") {
				t.Fatal("premise: initial summary lost decision")
			}
			// Legacy persisted sessions used a system-role continuation.
			seed.CompactedMessages[0].Role = role
			history := append(seed.CompactedMessages, bigTranscript()...)
			before, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			inject := memoryPreCompactInjector(context.Background(), store, ref, []string{"BRIEF.md"})
			called := false
			out := compactBetweenIterations(context.Background(), history, GenerationOptions{
				Model: "test-only-unknown-model", CompactPreserveRecent: 2,
				OnBeforeCompact: func(messages []api.Message) []api.Message { called = true; return inject(messages) },
			})
			if !called || len(out) >= len(history) {
				t.Fatal("premise: memory hook and compaction must run")
			}
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "UNIQUE_EARLIER_DECISION") {
				t.Fatalf("previous continuation re-summarized and lost its decision: %s", raw)
			}
			if !strings.Contains(out[0].Content[0].Text, "workspace_memory_index") {
				t.Fatalf("memory injection is absent from the new summary: %s", out[0].Content[0].Text)
			}
			after, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("compaction hook mutated the live history")
			}
		})
	}
}

func TestMemoryPreCompactInjector_EmptyHistory(t *testing.T) {
	store, ref, scope := memTest(t, "empty-history")
	if err := scope.Write("BRIEF.md", []byte("fresh memory")); err != nil {
		t.Fatal(err)
	}
	got := memoryPreCompactInjector(context.Background(), store, ref, []string{"BRIEF.md"})(nil)
	if len(got) != 1 || !got[0].IsInjected || !strings.Contains(got[0].Content[0].Text, "fresh memory") {
		t.Fatalf("empty history injection: %+v", got)
	}
}
