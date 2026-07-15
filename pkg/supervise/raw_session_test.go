package supervise

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The transcript observer must synthesize tool_called for a tool_use,
// tool_error for a failed tool_result (tagged with the tool name), and a
// turn-boundary llm_step_finished for a final assistant text message.
func TestTranscriptObserverSynthesizesEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	lines := []string{
		`{"type":"assistant","uuid":"u1","timestamp":"2026-06-25T10:00:00Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-25T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"FAIL"}]}}`,
		`{"type":"assistant","uuid":"u3","timestamp":"2026-06-25T10:00:02Z","message":{"content":[{"type":"text","text":"All done."}]}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	obs := NewTranscriptObserver(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, release, err := obs.ObserveRun(ctx, "")
	if err != nil {
		t.Fatalf("ObserveRun: %v", err)
	}
	defer release()

	var got []*store.Event
	deadline := time.After(10 * time.Second)
	for len(got) < 3 {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-deadline:
			t.Fatalf("got %d events, want 3: %+v", len(got), got)
		}
	}
	if got[0].Type != store.EventToolCalled || eventToolName(got[0]) != "Bash" {
		t.Errorf("event0=%s tool=%s; want tool_called Bash", got[0].Type, eventToolName(got[0]))
	}
	if got[1].Type != store.EventToolError || eventToolName(got[1]) != "Bash" {
		t.Errorf("event1=%s tool=%s; want tool_error Bash", got[1].Type, eventToolName(got[1]))
	}
	if got[2].Type != store.EventLLMStepFinished {
		t.Errorf("event2=%s; want llm_step_finished (turn boundary)", got[2].Type)
	}
}

// Inject then drain round-trips through the file-backed inbox keyed by
// project + session.
func TestInboxInjectorRoundTrip(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	ctx := context.Background()
	const key, sess = "-home-jo-proj", "sess-1"

	inj, err := NewInboxInjector(key, sess)
	if err != nil {
		t.Fatalf("NewInboxInjector: %v", err)
	}
	if err := inj.Inject(ctx, "", "", "fix the import"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	texts, err := DrainClaudeInbox(ctx, key, sess)
	if err != nil {
		t.Fatalf("DrainClaudeInbox: %v", err)
	}
	if len(texts) != 1 || texts[0] != "fix the import" {
		t.Fatalf("drained %v; want [fix the import]", texts)
	}
	// Second drain is empty (already delivered).
	again, _ := DrainClaudeInbox(ctx, key, sess)
	if len(again) != 0 {
		t.Fatalf("second drain returned %v; want empty", again)
	}
}

// Install must be non-destructive (unrelated keys + hooks preserved) and
// idempotent; uninstall removes only our entries.
func TestHookInstallNonDestructive(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing settings with an unrelated key + a foreign PostToolUse hook.
	pre := map[string]any{
		"permissions": map[string]any{"allow": []any{"Bash"}},
		"hooks": map[string]any{
			"PostToolUse": []any{
				map[string]any{"matcher": "Edit", "hooks": []any{map[string]any{"type": "command", "command": "my-formatter"}}},
			},
		},
	}
	writeJSON(t, path, pre)

	if _, changed, err := InstallHook(repo, HookScopeLocal); err != nil || !changed {
		t.Fatalf("InstallHook changed=%v err=%v", changed, err)
	}
	if !HookInstalled(repo, HookScopeLocal) {
		t.Fatal("HookInstalled false after install")
	}
	// Idempotent.
	if _, changed, _ := InstallHook(repo, HookScopeLocal); changed {
		t.Fatal("second InstallHook reported a change")
	}
	// The foreign formatter hook + unrelated key survive.
	got := readJSON(t, path)
	if _, ok := got["permissions"]; !ok {
		t.Error("permissions key was dropped")
	}
	if !settingsContains(got, "my-formatter") {
		t.Error("foreign formatter hook was dropped")
	}
	if !settingsContains(got, hookDrainSubcommand) {
		t.Error("drain hook not present")
	}

	// Uninstall removes only ours.
	if _, changed, err := UninstallHook(repo, HookScopeLocal); err != nil || !changed {
		t.Fatalf("UninstallHook changed=%v err=%v", changed, err)
	}
	got = readJSON(t, path)
	if settingsContains(got, hookDrainSubcommand) {
		t.Error("drain hook still present after uninstall")
	}
	if !settingsContains(got, "my-formatter") {
		t.Error("foreign formatter hook was removed by uninstall")
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func settingsContains(root map[string]any, substr string) bool {
	data, _ := json.Marshal(root)
	return strings.Contains(string(data), substr)
}

// ClaudeProjectKey must match Claude Code's ~/.claude/projects/<key>/
// naming (every non-alphanumeric char → '-'), which differs from
// store.EncodeWorkDirKey for any path containing a dot.
func TestClaudeProjectKey(t *testing.T) {
	cases := map[string]string{
		"/tmp/claude-rawsess.W9PWCo": "-tmp-claude-rawsess-W9PWCo",
		"/home/jo/.iterion":          "-home-jo--iterion",
		"/home/jo/lab/ai/iterion":    "-home-jo-lab-ai-iterion",
		"/a/b+c_d":                   "-a-b-c-d",
	}
	for in, want := range cases {
		if got := ClaudeProjectKey(in); got != want {
			t.Errorf("ClaudeProjectKey(%q) = %q; want %q", in, got, want)
		}
	}
}
