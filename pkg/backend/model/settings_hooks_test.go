package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestRegisterSettingsHooks_BlocksOnExit2(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A PreToolUse command hook matching Bash that denies (exit 2).
	settings := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo nope >&2; exit 2"}]}]}}`
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	r := hooks.NewRunner()
	if n := registerSettingsHooks(r, ws, nil); n != 1 {
		t.Fatalf("registerSettingsHooks wired %d events, want 1", n)
	}

	// Fire PreToolUse for Bash → must Block with the hook's stderr as reason.
	dec, err := r.Fire(context.Background(), hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": "ls"},
	})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if dec.Action != hooks.ActionBlock {
		t.Fatalf("Action = %v, want Block", dec.Action)
	}

	// A non-matching tool name continues.
	dec2, _ := r.Fire(context.Background(), hooks.Context{
		Event:    hooks.PreToolUse,
		ToolName: "Read",
	})
	if dec2.Action != hooks.ActionContinue {
		t.Fatalf("non-matching tool Action = %v, want Continue", dec2.Action)
	}
}

func TestRegisterSettingsHooks_NoFile(t *testing.T) {
	r := hooks.NewRunner()
	if n := registerSettingsHooks(r, t.TempDir(), nil); n != 0 {
		t.Fatalf("no settings.json should wire 0 events, got %d", n)
	}
}

// TestRegisterSettingsHooksJSON_BridgesAllExportedEvents: every event
// claw-code-go exports is wired from the document, and a UserPromptSubmit
// command hook receives the submitted prompt in HOOK_USER_PROMPT.
func TestRegisterSettingsHooksJSON_BridgesAllExportedEvents(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "prompt.txt")
	doc := json.RawMessage(`{
		"PreToolUse":         [{"hooks": [{"type": "command", "command": "true"}]}],
		"PostToolUse":        [{"hooks": [{"type": "command", "command": "true"}]}],
		"PostToolUseFailure": [{"hooks": [{"type": "command", "command": "true"}]}],
		"UserPromptSubmit":   [{"hooks": [{"type": "command", "command": "printf %s \"$HOOK_USER_PROMPT\" > ` + marker + `"}]}],
		"PreCompact":         [{"hooks": [{"type": "command", "command": "true"}]}],
		"PostCompact":        [{"hooks": [{"type": "command", "command": "true"}]}],
		"Stop":               [{"hooks": [{"type": "command", "command": "true"}]}]
	}`)
	r := hooks.NewRunner()
	if n := registerSettingsHooksJSON(r, doc, nil); n != 7 {
		t.Fatalf("registerSettingsHooksJSON wired %d events, want 7", n)
	}
	if _, err := r.Fire(context.Background(), hooks.Context{
		Event:      hooks.UserPromptSubmit,
		UserPrompt: "ship it",
	}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("UserPromptSubmit hook did not run: %v", err)
	}
	if string(data) != "ship it" {
		t.Fatalf("HOOK_USER_PROMPT = %q, want %q", data, "ship it")
	}
}

// TestRegisterSettingsHooksJSON_UnmappedEventWarns: an event claw does not
// export (SessionStart) wires nothing AND is named in a warning — never
// dropped in silence.
func TestRegisterSettingsHooksJSON_UnmappedEventWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	doc := json.RawMessage(`{
		"SessionStart": [{"hooks": [{"type": "command", "command": "true"}]}],
		"Stop":         [{"hooks": [{"type": "command", "command": "true"}]}]
	}`)
	r := hooks.NewRunner()
	if n := registerSettingsHooksJSON(r, doc, logger); n != 1 {
		t.Fatalf("wired %d events, want 1 (Stop only)", n)
	}
	if !strings.Contains(buf.String(), "SessionStart") {
		t.Fatalf("unmapped event not named in diagnostics, got: %q", buf.String())
	}
}

// TestRegisterSettingsHooksJSON_NonCommandEntryWarns: prompt-type entries are
// claude_code's native surface — skipped on claw, with a diagnostic.
func TestRegisterSettingsHooksJSON_NonCommandEntryWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	doc := json.RawMessage(`{
		"PreToolUse": [{"hooks": [
			{"type": "prompt", "prompt": "is this safe?"},
			{"type": "command", "command": "true"}
		]}]
	}`)
	r := hooks.NewRunner()
	if n := registerSettingsHooksJSON(r, doc, logger); n != 1 {
		t.Fatalf("wired %d events, want 1", n)
	}
	if !strings.Contains(buf.String(), `"prompt"`) {
		t.Fatalf("non-command entry not named in diagnostics, got: %q", buf.String())
	}
}

// TestRegisterSettingsHooksJSON_PreCompactBlock: a command hook's exit 2 on a
// compaction event maps to a Block decision, same convention as PreToolUse.
func TestRegisterSettingsHooksJSON_PreCompactBlock(t *testing.T) {
	doc := json.RawMessage(`{
		"PreCompact": [{"hooks": [{"type": "command", "command": "echo hold >&2; exit 2"}]}]
	}`)
	r := hooks.NewRunner()
	if n := registerSettingsHooksJSON(r, doc, nil); n != 1 {
		t.Fatalf("wired %d events, want 1", n)
	}
	dec, err := r.Fire(context.Background(), hooks.Context{Event: hooks.PreCompact, MessageCount: 42})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if dec.Action != hooks.ActionBlock {
		t.Fatalf("Action = %v, want Block", dec.Action)
	}
	if !strings.Contains(dec.Reason, "hold") {
		t.Fatalf("Reason = %q, want the hook's stderr", dec.Reason)
	}
}

func TestReadSettingsHooksDoc(t *testing.T) {
	// Missing file → nil, nil.
	if doc, err := readSettingsHooksDoc(t.TempDir()); err != nil || doc != nil {
		t.Fatalf("missing file: doc=%v err=%v, want nil, nil", doc, err)
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No hooks key → nil.
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if doc, err := readSettingsHooksDoc(ws); err != nil || doc != nil {
		t.Fatalf("no hooks key: doc=%v err=%v, want nil, nil", doc, err)
	}
	// Invalid JSON → error (the caller warns; a hooks file that cannot be
	// parsed must not vanish in silence).
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSettingsHooksDoc(ws); err == nil {
		t.Fatal("invalid JSON: want error")
	}
	// Valid → the raw hooks object.
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"),
		[]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := readSettingsHooksDoc(ws)
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if !strings.Contains(string(doc), "Stop") {
		t.Fatalf("doc = %s, want the hooks object", doc)
	}
}

// TestRegisterSettingsHooksJSON_PreCompactAutoMatcherFires is the HIGH
// mutation test: the documented `"matcher": "auto"` on PreCompact MUST
// fire. Before the per-event matcher split it evaluated the regex "auto"
// against an empty tool name — a documented configuration dead in silence.
func TestRegisterSettingsHooksJSON_PreCompactAutoMatcherFires(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "compact.txt")
	doc := json.RawMessage(`{
		"PreCompact": [{"matcher": "auto", "hooks": [{"type": "command", "command": "touch ` + marker + `"}]}]
	}`)
	r := hooks.NewRunner()
	if n := registerSettingsHooksJSON(r, doc, nil); n != 1 {
		t.Fatalf("wired %d events, want 1", n)
	}
	if _, err := r.Fire(context.Background(), hooks.Context{Event: hooks.PreCompact, MessageCount: 10}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(`"matcher": "auto" on PreCompact did not fire the hook`)
	}
}

// TestRegisterSettingsHooksJSON_CompactManualMatcherNeverFires: claw has no
// manual compaction, so a "manual" matcher never matches — and says so.
func TestRegisterSettingsHooksJSON_CompactManualMatcherNeverFires(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "compact.txt")
	var buf bytes.Buffer
	doc := json.RawMessage(`{
		"PreCompact": [{"matcher": "manual", "hooks": [{"type": "command", "command": "touch ` + marker + `"}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, iterlog.New(iterlog.LevelWarn, &buf))
	if _, err := r.Fire(context.Background(), hooks.Context{Event: hooks.PreCompact, MessageCount: 10}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal(`"matcher": "manual" fired on claw, which has no manual compaction`)
	}
	if !strings.Contains(buf.String(), "manual") {
		t.Fatalf("manual matcher not named in diagnostics, got: %q", buf.String())
	}
	// An unknown compaction source never matches either, with a diagnostic.
	buf.Reset()
	doc2 := json.RawMessage(`{
		"PostCompact": [{"matcher": "weekly", "hooks": [{"type": "command", "command": "true"}]}]
	}`)
	r2 := hooks.NewRunner()
	registerSettingsHooksJSON(r2, doc2, iterlog.New(iterlog.LevelWarn, &buf))
	if !strings.Contains(buf.String(), "weekly") {
		t.Fatalf("unknown compaction source not named, got: %q", buf.String())
	}
}

// TestRegisterSettingsHooksJSON_MatcherOnNoMatcherEventIgnored: claude_code
// defines no matcher for UserPromptSubmit/Stop — the hook always fires, and
// the ignored matcher is named at registration.
func TestRegisterSettingsHooksJSON_MatcherOnNoMatcherEventIgnored(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "prompt.txt")
	var buf bytes.Buffer
	// A matcher that could never match this event's (empty) tool name —
	// the hook must fire anyway.
	doc := json.RawMessage(`{
		"UserPromptSubmit": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "touch ` + marker + `"}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, iterlog.New(iterlog.LevelWarn, &buf))
	if _, err := r.Fire(context.Background(), hooks.Context{Event: hooks.UserPromptSubmit, UserPrompt: "hi"}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("UserPromptSubmit hook with a matcher did not fire — the matcher must be ignored on this event")
	}
	if !strings.Contains(buf.String(), "ignored") {
		t.Fatalf("ignored matcher not named in diagnostics, got: %q", buf.String())
	}
}

// TestRegisterSettingsHooksJSON_InvalidRegexMatcherWarns: an invalid regex
// on a tool event matches nothing — named at registration, not silent.
func TestRegisterSettingsHooksJSON_InvalidRegexMatcherWarns(t *testing.T) {
	var buf bytes.Buffer
	doc := json.RawMessage(`{
		"PreToolUse": [{"matcher": "(", "hooks": [{"type": "command", "command": "true"}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, iterlog.New(iterlog.LevelWarn, &buf))
	if !strings.Contains(buf.String(), "not a valid regex") {
		t.Fatalf("invalid regex not named in diagnostics, got: %q", buf.String())
	}
}

// TestRunCommandHook_TruncatesHugePrompt is the E2BIG mutation test: a
// 256 KiB prompt must still reach the screening hook — truncated, with an
// explicit marker — rather than failing the exec (a silent fail-open).
func TestRunCommandHook_TruncatesHugePrompt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "prompt.txt")
	doc := json.RawMessage(`{
		"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "printf %s \"$HOOK_USER_PROMPT\" > ` + marker + `"}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, nil)
	big := strings.Repeat("x", 256*1024)
	if _, err := r.Fire(context.Background(), hooks.Context{Event: hooks.UserPromptSubmit, UserPrompt: big}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("hook did not run on a 256 KiB prompt (E2BIG fail-open): %v", err)
	}
	if !strings.Contains(string(data), "hook payload truncated") {
		t.Fatalf("truncated payload carries no marker (%d bytes received)", len(data))
	}
	if len(data) > hookEnvMaxBytes+200 {
		t.Fatalf("hook received %d bytes, want ≤ the cap plus the marker", len(data))
	}
}

// TestRunCommandHook_NonZeroExitLogged: a hook failing with an exit other
// than 2 continues — but is logged, not silent.
func TestRunCommandHook_NonZeroExitLogged(t *testing.T) {
	var buf bytes.Buffer
	doc := json.RawMessage(`{
		"PostToolUse": [{"hooks": [{"type": "command", "command": "echo broken >&2; exit 1"}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, iterlog.New(iterlog.LevelWarn, &buf))
	dec, err := r.Fire(context.Background(), hooks.Context{Event: hooks.PostToolUse, ToolName: "Bash"})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if dec.Action != hooks.ActionContinue {
		t.Fatalf("Action = %v, want Continue", dec.Action)
	}
	if !strings.Contains(buf.String(), "broken") {
		t.Fatalf("failing hook not logged, got: %q", buf.String())
	}
}

// TestSettingsHooksForWire_WrongTypeDocWarns: a hooks document of the wrong
// JSON type (`"hooks": []`) on the sandboxed path is named on the LAUNCHER —
// the only channel a successful sandboxed run has — and not shipped.
func TestSettingsHooksForWire_WrongTypeDocWarns(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(`{"hooks": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	doc := settingsHooksForWire(ws, "n1", 0, iterlog.New(iterlog.LevelWarn, &buf))
	if doc != nil {
		t.Fatalf("doc = %s, want nil for an unparseable document", doc)
	}
	if !strings.Contains(buf.String(), "fires no settings hooks") {
		t.Fatalf("wrong-type document not named on the launcher, got: %q", buf.String())
	}
}

// TestSettingsHooksForWire_ValidDocShippedAndDiagnosed: the valid document
// crosses, with its unbridgeable entries named on the launcher.
func TestSettingsHooksForWire_ValidDocShippedAndDiagnosed(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"true"}]}],"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`
	if err := os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	doc := settingsHooksForWire(ws, "n1", 0, iterlog.New(iterlog.LevelWarn, &buf))
	if !strings.Contains(string(doc), "Stop") {
		t.Fatalf("doc = %s, want the hooks document shipped", doc)
	}
	if !strings.Contains(buf.String(), "SessionStart") {
		t.Fatalf("unmapped event not named on the launcher, got: %q", buf.String())
	}
	// No settings file → nil, no warning.
	buf.Reset()
	if doc := settingsHooksForWire(t.TempDir(), "n1", 0, iterlog.New(iterlog.LevelWarn, &buf)); doc != nil || buf.Len() != 0 {
		t.Fatalf("missing file: doc=%v warn=%q, want nil and silence", doc, buf.String())
	}
}

// TestRunCommandHook_OwnTimeoutWarns: the hook's OWN timeout (parent ctx
// alive) is named.
func TestRunCommandHook_OwnTimeoutWarns(t *testing.T) {
	var buf bytes.Buffer
	doc := json.RawMessage(`{
		"PostToolUse": [{"hooks": [{"type": "command", "command": "sleep 10", "timeout": 1}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, iterlog.New(iterlog.LevelWarn, &buf))
	if _, err := r.Fire(context.Background(), hooks.Context{Event: hooks.PostToolUse, ToolName: "Bash"}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if !strings.Contains(buf.String(), "timed out") {
		t.Fatalf("own timeout not logged, got: %q", buf.String())
	}
}

// TestRunCommandHook_ParentCancelNotReportedAsTimeout: when the RUN is
// cancelled, the hook the cancellation kills must not be misreported as a
// hook timeout — the operator interrupted the run, the hook failed nothing.
func TestRunCommandHook_ParentCancelNotReportedAsTimeout(t *testing.T) {
	var buf bytes.Buffer
	doc := json.RawMessage(`{
		"PostToolUse": [{"hooks": [{"type": "command", "command": "sleep 10"}]}]
	}`)
	r := hooks.NewRunner()
	registerSettingsHooksJSON(r, doc, iterlog.New(iterlog.LevelWarn, &buf))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	// Fire legitimately surfaces the parent cancellation; the behaviour
	// under test is the LOG line, not Fire's error.
	if _, err := r.Fire(ctx, hooks.Context{Event: hooks.PostToolUse, ToolName: "Bash"}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Fire: %v", err)
	}
	if strings.Contains(buf.String(), "timed out") {
		t.Fatalf("parent cancellation misreported as a hook timeout, got: %q", buf.String())
	}
}
