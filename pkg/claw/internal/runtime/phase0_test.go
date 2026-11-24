package runtime

import (
	"context"
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/hooks"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/hooks/hookstesting"
	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"strings"
	"testing"
)

// --- BUG-TOOL-CALL-COUNT: Tool call count increment tests ---

func TestConversationLoop_ToolCallCount_Empty(t *testing.T) {
	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
	}
	if got := loop.ToolCallCount(); got != 0 {
		t.Errorf("ToolCallCount() on fresh loop = %d, want 0", got)
	}
}

func TestConversationLoop_ToolCallCount_Increment(t *testing.T) {
	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
	}

	// Execute 3 tools (use unknown tools — they fail but still increment the counter).
	// The counter should track all execution attempts.
	for range 3 {
		loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo hi"})
	}

	if got := loop.ToolCallCount(); got != 3 {
		t.Errorf("ToolCallCount() after 3 calls = %d, want 3", got)
	}
}

func TestLoopAdapter_ToolCallCount_ReadsFromLoop(t *testing.T) {
	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
	}

	// Execute 2 tools.
	loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo 1"})
	loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo 2"})

	adapter := NewLoopAdapter(loop)
	if got := adapter.ToolCallCount(); got != 2 {
		t.Errorf("adapter.ToolCallCount() = %d, want 2", got)
	}
}

func TestLoopAdapter_ToolCallCount_NilLoop(t *testing.T) {
	adapter := NewLoopAdapter(nil)
	if got := adapter.ToolCallCount(); got != 0 {
		t.Errorf("adapter.ToolCallCount() on nil loop = %d, want 0", got)
	}
}

// --- BUG-INJECT-PROMPT-FLAG: IsInjected flag tests ---

func TestMessage_IsInjected_OmitEmpty(t *testing.T) {
	// Non-injected message should omit the field from JSON.
	msg := api.Message{
		Role:    "user",
		Content: []api.ContentBlock{{Type: "text", Text: "hello"}},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "is_injected") {
		t.Errorf("non-injected message JSON should omit is_injected, got: %s", s)
	}
}

func TestMessage_IsInjected_Present(t *testing.T) {
	// Injected message should include the field in JSON.
	msg := api.Message{
		Role:       "user",
		Content:    []api.ContentBlock{{Type: "text", Text: "injected"}},
		IsInjected: true,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"is_injected":true`) {
		t.Errorf("injected message JSON should include is_injected:true, got: %s", s)
	}
}

func TestMessage_IsInjected_Deserialize_Missing(t *testing.T) {
	// Old sessions without is_injected should deserialize cleanly.
	raw := `{"role":"user","content":[{"type":"text","text":"hello"}]}`
	var msg api.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.IsInjected {
		t.Error("missing is_injected should default to false")
	}
}

func TestMessage_IsInjected_Deserialize_True(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"text","text":"injected"}],"is_injected":true}`
	var msg api.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.IsInjected {
		t.Error("is_injected:true should deserialize correctly")
	}
}

func TestInjectPrompt_SetsIsInjected(t *testing.T) {
	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
		Session:     NewSession(),
	}
	adapter := NewLoopAdapter(loop)

	if err := adapter.InjectPrompt("test injection"); err != nil {
		t.Fatal(err)
	}

	if len(loop.Session.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(loop.Session.Messages))
	}
	if !loop.Session.Messages[0].IsInjected {
		t.Error("injected message should have IsInjected=true")
	}
}

func TestCountRealUserTurns_ExcludesInjected(t *testing.T) {
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "real 1"}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "injected"}}, IsInjected: true},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "reply"}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "real 2"}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "injected 2"}}, IsInjected: true},
	}

	got := CountRealUserTurns(messages)
	if got != 2 {
		t.Errorf("CountRealUserTurns = %d, want 2", got)
	}
}

func TestCountRealUserTurns_Empty(t *testing.T) {
	if got := CountRealUserTurns(nil); got != 0 {
		t.Errorf("CountRealUserTurns(nil) = %d, want 0", got)
	}
}

// --- WIRE-BUILD-VARS: Build version/commit wiring tests ---

func TestLoopAdapter_Version_Default(t *testing.T) {
	adapter := NewLoopAdapter(nil)
	if got := adapter.Version(); got != "dev" {
		t.Errorf("Version() default = %q, want %q", got, "dev")
	}
}

func TestLoopAdapter_Version_Wired(t *testing.T) {
	adapter := NewLoopAdapter(nil)
	adapter.SetBuildInfo("v1.2.3", "abc1234")
	if got := adapter.Version(); got != "v1.2.3" {
		t.Errorf("Version() = %q, want %q", got, "v1.2.3")
	}
	if got := adapter.Commit(); got != "abc1234" {
		t.Errorf("Commit() = %q, want %q", got, "abc1234")
	}
}

func TestLoopAdapter_BuildInfo_ViaLoop(t *testing.T) {
	loop := &ConversationLoop{
		Permissions:  DefaultPermissions(),
		Config:       &Config{},
		Session:      NewSession(),
		BuildVersion: "v2.0.0",
		BuildCommit:  "deadbeef",
	}
	adapter := NewLoopAdapter(loop)
	adapter.SetBuildInfo(loop.BuildVersion, loop.BuildCommit)
	if got := adapter.Version(); got != "v2.0.0" {
		t.Errorf("Version() from loop = %q, want %q", got, "v2.0.0")
	}
	if got := adapter.Commit(); got != "deadbeef" {
		t.Errorf("Commit() from loop = %q, want %q", got, "deadbeef")
	}
}

// --- WIRE-HOOK-PERM-OVERRIDE: Hook permission override tests ---

func TestExecuteTool_HookPermissionDeny(t *testing.T) {
	testutil.IsolateShellHome(t)
	deny := hooks.PermissionDeny
	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
		HookRunner:  hookstesting.NewHookRunnerWithOverride(&deny, "policy says no"),
	}

	result := loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo hi"})
	if !result.IsError {
		t.Error("hook deny should produce error result")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "policy says no") {
		t.Errorf("deny reason not in result: %+v", result)
	}
}

// QUALITY-1: Hook "allow" with a matching ask-rule should NOT remember
// as auto-allowed. The ask-rule must take precedence, matching Rust semantics.
func TestExecuteTool_HookAllowRespectsAskRules(t *testing.T) {
	testutil.IsolateShellHome(t)
	allow := hooks.PermissionAllow

	// Create a manager with an ask-rule that matches "bash".
	mgr := permissions.NewManager(permissions.ModeDefault, &permissions.Ruleset{
		Rules: []permissions.Rule{
			{Tool: "bash", Pattern: "", Decision: permissions.DecisionAsk, RawDecision: "ask"},
		},
	})

	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
		HookRunner:  hookstesting.NewHookRunnerWithOverride(&allow, "hook says ok"),
		PermManager: mgr,
	}

	// Execute — since ask-rule matches, the allow should NOT be remembered.
	// The tool will still execute (no interactive prompt wired up in unit test),
	// but the cache should not contain a remembered allow for bash.
	_ = loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo hi"})

	// Verify the ask-rule prevented auto-remembering: Check() should still
	// return Ask (not Allow) since no decision was cached.
	decision := mgr.Check("bash", "echo hi")
	if decision != permissions.DecisionAsk {
		t.Errorf("after hook allow + ask-rule match, Check() = %d, want DecisionAsk (%d)", decision, permissions.DecisionAsk)
	}
}

// QUALITY-1: Hook "allow" WITHOUT a matching ask-rule should remember as allowed.
func TestExecuteTool_HookAllowRemembersWithoutAskRule(t *testing.T) {
	testutil.IsolateShellHome(t)
	allow := hooks.PermissionAllow

	// Manager with no ask-rules.
	mgr := permissions.NewManager(permissions.ModeDefault, &permissions.Ruleset{})

	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
		HookRunner:  hookstesting.NewHookRunnerWithOverride(&allow, "hook says ok"),
		PermManager: mgr,
	}

	_ = loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo hi"})

	// Without ask-rules, the allow should be remembered.
	decision := mgr.Check("bash", "echo hi")
	if decision != permissions.DecisionAllow {
		t.Errorf("after hook allow (no ask-rule), Check() = %d, want DecisionAllow (%d)", decision, permissions.DecisionAllow)
	}
}

// QUALITY-2: Hook "ask" should not auto-allow (falls through to normal check).
func TestExecuteTool_HookAskDoesNotAutoAllow(t *testing.T) {
	testutil.IsolateShellHome(t)
	ask := hooks.PermissionAsk

	mgr := permissions.NewManager(permissions.ModeDefault, &permissions.Ruleset{})

	loop := &ConversationLoop{
		Permissions: DefaultPermissions(),
		Config:      &Config{},
		HookRunner:  hookstesting.NewHookRunnerWithOverride(&ask, "hook wants confirmation"),
		PermManager: mgr,
	}

	_ = loop.ExecuteTool(context.Background(), "bash", map[string]any{"command": "echo hi"})

	// Ask override should not cache any decision. Probe with a mutating
	// command: read-only ones (like echo) are now legitimately auto-allowed
	// by the built-in safe-list regardless of the cache.
	decision := mgr.Check("bash", "touch hi.txt")
	if decision != permissions.DecisionAsk {
		t.Errorf("after hook ask, Check() = %d, want DecisionAsk (%d)", decision, permissions.DecisionAsk)
	}
}

// --- QUALITY-4: ShouldCompact with real-turn gating ---

func TestShouldCompact_InjectedMessagesDoNotTrigger(t *testing.T) {
	cfg := &Config{
		CompactionEnabled:   true,
		MaxTokens:           100,
		CompactionThreshold: 0.5, // 50 tokens threshold
	}

	// 1 real user turn + several injected messages. Token count exceeds the
	// threshold, but only 1 real turn exists (< minRealTurnsForCompaction=2).
	longText := strings.Repeat("x", 200)
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: longText}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: longText}}, IsInjected: true},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: longText}}, IsInjected: true},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: longText}}, IsInjected: true},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: longText}}},
	}

	if ShouldCompact(0, messages, cfg) {
		t.Error("ShouldCompact should return false when real user turn count < minRealTurnsForCompaction")
	}
}

func TestShouldCompact_EnoughRealTurnsTriggers(t *testing.T) {
	cfg := &Config{
		CompactionEnabled:   true,
		MaxTokens:           100,
		CompactionThreshold: 0.5, // 50 tokens threshold
	}

	// 2 real user turns + enough tokens = should compact.
	longText := strings.Repeat("x", 200)
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: longText}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: longText}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: longText}}},
	}

	if !ShouldCompact(0, messages, cfg) {
		t.Error("ShouldCompact should return true when real turn count >= minRealTurnsForCompaction and tokens exceed threshold")
	}
}
