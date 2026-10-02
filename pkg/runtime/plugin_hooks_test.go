package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// writeHookyPlugin installs an enabled plugin "hooky" under ITERION_HOME that
// contributes one PreToolUse command hook, and returns nothing (state via env).
func writeHookyPlugin(t *testing.T, home string, enabled bool) {
	t.Helper()
	dir := filepath.Join(home, "plugins", "hooky")
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: hooky\ncontributes:\n  hooks:\n    - hooks/h.json\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	frag := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "hooks", "h.json"), []byte(frag), 0o644); err != nil {
		t.Fatal(err)
	}
	state := "enabled:\n  hooky: false\n"
	if enabled {
		state = "enabled:\n  hooky: true\n"
	}
	if err := os.WriteFile(filepath.Join(home, "plugins.yaml"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
}

func preToolUseLen(t *testing.T, settingsPath string) int {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return 0
	}
	var s struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return len(s.Hooks["PreToolUse"])
}

func TestMergePluginHooks_InjectIdempotentRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".claude", "settings.json")

	// A pre-existing USER hook must be preserved across merges.
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	userSettings := `{"hooks":{"PreToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"user-hook"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	writeHookyPlugin(t, home, true)

	// First merge: user hook + plugin hook = 2.
	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 2 {
		t.Fatalf("after inject: PreToolUse = %d, want 2 (user + plugin)", n)
	}

	// Second merge (resume/re-run): must NOT duplicate — still 2.
	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 2 {
		t.Fatalf("after re-inject: PreToolUse = %d, want 2 (idempotent)", n)
	}

	// Disable the plugin, merge again: plugin hook removed, user hook kept = 1.
	writeHookyPlugin(t, home, false)
	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge 3: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 1 {
		t.Fatalf("after disable: PreToolUse = %d, want 1 (user hook only)", n)
	}
}

// A malformed (but existing) user settings.json must never be destroyed by the
// merge: mergePluginHooks returns an error and leaves the file bytes intact.
func TestMergePluginHooks_MalformedSettingsRefusesRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := []byte(`{"permissions":`)
	if err := os.WriteFile(settingsPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	writeHookyPlugin(t, home, true)

	if _, err := mergePluginHooks(ws, nil); err == nil {
		t.Fatal("want error for malformed settings.json, got nil")
	}
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read back settings.json: %v", err)
	}
	if !bytes.Equal(got, malformed) {
		t.Fatalf("settings.json was rewritten despite parse error:\n got: %s\nwant: %s", got, malformed)
	}
}

// A settings.json containing literal `null` unmarshals into a nil map without
// error; the merge must treat it as empty (no panic, no error) and proceed.
func TestMergePluginHooks_NullSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("null\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeHookyPlugin(t, home, true)

	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge over null settings: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 1 {
		t.Fatalf("after merge over null: PreToolUse = %d, want 1 (plugin hook)", n)
	}
}

// addPostToolUseHook rewrites the installed plugin's fragment with a second
// event, so the next merge's hook set differs from what the sidecar recorded
// — the "hook set changed during the pause" window.
func addPostToolUseHook(t *testing.T, home string) {
	t.Helper()
	frag := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}],"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo done"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "plugins", "hooky", "hooks", "h.json"), []byte(frag), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agentEditSettings adds a permissions block to an existing settings.json —
// the previous run segment's deliverable on a repository tracking the file.
func agentEditSettings(t *testing.T, settingsPath string) {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["permissions"] = map[string]any{"allow": []any{}}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func settingsHasPermissions(t *testing.T, settingsPath string) bool {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	var s struct {
		Permissions map[string]any `json:"permissions"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return s.Permissions != nil
}

// hookLen is preToolUseLen generalized to any event.
func hookLen(t *testing.T, settingsPath, event string) int {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return 0
	}
	var s struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return len(s.Hooks[event])
}

// hooksResumeFixture is a repository TRACKING `.claude/settings.json`, one
// enabled hook plugin, and the run-start merge already applied: the manifest
// recorded the injected file, exactly as a launch leaves it.
func hooksResumeFixture(t *testing.T) (home, ws, settingsPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws = t.TempDir()
	gittest.InitRepo(t, ws)
	settingsPath = filepath.Join(ws, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, ws, filepath.Join(".claude", "settings.json"), "{}\n", "track the repo's own settings")
	writeHookyPlugin(t, home, true)
	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("run-start merge: %v", err)
	}
	return home, ws, settingsPath
}

// R213476 — the #1571 destruction surviving on the resume paths: the run's
// deliverable is the edit of a TRACKED `.claude/settings.json`; the segment
// pauses after the edit; the resume's hooks merge runs with an UNCHANGED
// hook set. Pre-fix it rewrote the file unconditionally and re-recorded the
// hash, so the finalize probe read the run's own edit as mirror noise and
// the worktree removal destroyed it. The no-op merge must not touch the
// file at all, and the edit must read as work.
func TestMergePluginHooksOnResumeKeepsAnAgentEditAsWork(t *testing.T) {
	_, ws, settingsPath := hooksResumeFixture(t)
	agentEditSettings(t, settingsPath)

	// The resume: same hook set, merge replayed.
	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("resume merge: %v", err)
	}

	porcelain, err := runGit(ws, "status", "--porcelain", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Fatalf("runOutputPaths = %q, want [.claude/settings.json] — the run's edit must survive the resume's merge as work", got)
	}
	if got := commitWorkPaths(ws, porcelain); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Fatalf("commitWorkPaths = %q, want [.claude/settings.json] — the operator's probe agrees", got)
	}
	if !settingsHasPermissions(t, settingsPath) {
		t.Fatal("the agent's edit was clobbered by the resume's merge")
	}
	if n := hookLen(t, settingsPath, "PreToolUse"); n != 1 {
		t.Fatalf("the injected hook is gone: PreToolUse = %d, want 1", n)
	}
}

// The same replay with NO edit in between: the merge is a semantic no-op
// and must not touch the file at all — mtime included — so a converged run
// never sees the engine's own injection as dirt (#1364 direction).
func TestMergePluginHooksNoOpMergeDoesNotTouchTheFile(t *testing.T) {
	_, ws, settingsPath := hooksResumeFixture(t)
	st1, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("resume merge: %v", err)
	}

	st2, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatalf("a no-op merge rewrote settings.json (mtime %v → %v)", st1.ModTime(), st2.ModTime())
	}
	porcelain, err := runGit(ws, "status", "--porcelain", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — the engine's unchanged injection is not work", got)
	}
}

// The residual window: the hook set CHANGED during the pause AND the agent
// edited the file. The engine must rewrite (its hooks are load-bearing) but
// must NOT record the result: the merged file carries the agent's edit, and
// unrecorded it reads as work and banks — the edit is not lost, it travels.
func TestMergePluginHooksChangedHooksPlusAgentEditRewritesWithoutRecording(t *testing.T) {
	home, ws, settingsPath := hooksResumeFixture(t)
	agentEditSettings(t, settingsPath)
	addPostToolUseHook(t, home)

	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("resume merge: %v", err)
	}

	if n := hookLen(t, settingsPath, "PostToolUse"); n != 1 {
		t.Fatalf("the changed hook set was not merged: PostToolUse = %d, want 1", n)
	}
	if !settingsHasPermissions(t, settingsPath) {
		t.Fatal("the agent's edit was clobbered by the merge")
	}
	porcelain, err := runGit(ws, "status", "--porcelain", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Fatalf("runOutputPaths = %q, want [.claude/settings.json] — the rewrite over a diverged file must NOT be recorded as the mirror's own", got)
	}
}

// The same changed hook set with NO agent edit: the rewrite is the mirror's
// own act, recorded — a converged run does not bank it. Characterization,
// not a mutation witness: pre-fix behaviour agrees here; what it pins is
// that the divergence respect does not overcorrect into "never record".
func TestMergePluginHooksChangedHooksWithoutAgentEditRecords(t *testing.T) {
	home, ws, settingsPath := hooksResumeFixture(t)
	addPostToolUseHook(t, home)

	if _, err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("resume merge: %v", err)
	}

	if n := hookLen(t, settingsPath, "PostToolUse"); n != 1 {
		t.Fatalf("the changed hook set was not merged: PostToolUse = %d, want 1", n)
	}
	porcelain, err := runGit(ws, "status", "--porcelain", "-z")
	if err != nil {
		t.Fatal(err)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — the engine's own rewrite with no edit in between is not the run's work", got)
	}
}
