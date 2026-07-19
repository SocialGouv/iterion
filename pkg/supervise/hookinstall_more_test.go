package supervise

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsPath(t *testing.T) {
	if got := settingsPath("/repo", HookScopeLocal); got != filepath.Join("/repo", ".claude", "settings.local.json") {
		t.Errorf("local path = %q", got)
	}
	if got := settingsPath("/repo", HookScopeProject); got != filepath.Join("/repo", ".claude", "settings.json") {
		t.Errorf("project path = %q", got)
	}
	// Any other scope value falls back to the local file.
	if got := settingsPath("/repo", HookSettingsScope("weird")); !strings.HasSuffix(got, "settings.local.json") {
		t.Errorf("unknown scope path = %q; want local fallback", got)
	}
}

// Installing into a repo with no .claude dir creates the settings file
// with a Stop hook (no matcher) and a PostToolUse hook (matcher "*").
func TestHookInstallFreshRepoShape(t *testing.T) {
	repo := t.TempDir()

	path, changed, err := InstallHook(repo, HookScopeLocal)
	if err != nil || !changed {
		t.Fatalf("InstallHook = (%q, %v, %v)", path, changed, err)
	}
	if path != settingsPath(repo, HookScopeLocal) {
		t.Errorf("returned path = %q", path)
	}
	root := readJSON(t, path)
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks key missing/mis-typed: %+v", root)
	}

	stop, ok := hooks["Stop"].([]any)
	if !ok || len(stop) != 1 {
		t.Fatalf("Stop hooks = %+v; want one block", hooks["Stop"])
	}
	stopBlock := stop[0].(map[string]any)
	if _, hasMatcher := stopBlock["matcher"]; hasMatcher {
		t.Error("Stop block must not carry a matcher")
	}
	if !blockHasOurCommand(stopBlock) {
		t.Error("Stop block missing the drain command")
	}

	post, ok := hooks["PostToolUse"].([]any)
	if !ok || len(post) != 1 {
		t.Fatalf("PostToolUse hooks = %+v; want one block", hooks["PostToolUse"])
	}
	postBlock := post[0].(map[string]any)
	if m, _ := postBlock["matcher"].(string); m != "*" {
		t.Errorf("PostToolUse matcher = %q; want *", m)
	}
	if !blockHasOurCommand(postBlock) {
		t.Error("PostToolUse block missing the drain command")
	}

	if !HookInstalled(repo, HookScopeLocal) {
		t.Error("HookInstalled = false after fresh install")
	}
}

// Uninstalling the only hooks removes the hooks key entirely, and
// uninstall on a missing settings file is a clean no-op.
func TestHookUninstallPrunesAndTolerates(t *testing.T) {
	repo := t.TempDir()

	// Missing file: no error, no change.
	if _, changed, err := UninstallHook(repo, HookScopeLocal); err != nil || changed {
		t.Fatalf("uninstall on missing file = (changed=%v, err=%v); want no-op", changed, err)
	}

	if _, _, err := InstallHook(repo, HookScopeLocal); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := UninstallHook(repo, HookScopeLocal); err != nil || !changed {
		t.Fatalf("uninstall = (changed=%v, err=%v)", changed, err)
	}
	root := readJSON(t, settingsPath(repo, HookScopeLocal))
	if _, ok := root["hooks"]; ok {
		t.Errorf("hooks key not pruned after removing our only hooks: %+v", root)
	}
	if HookInstalled(repo, HookScopeLocal) {
		t.Error("HookInstalled = true after uninstall")
	}
	// Second uninstall is a no-op.
	if _, changed, err := UninstallHook(repo, HookScopeLocal); err != nil || changed {
		t.Fatalf("second uninstall = (changed=%v, err=%v); want no-op", changed, err)
	}
}

func TestHookInstallProjectScopeIsSeparate(t *testing.T) {
	repo := t.TempDir()
	if _, changed, err := InstallHook(repo, HookScopeProject); err != nil || !changed {
		t.Fatalf("project install = (changed=%v, err=%v)", changed, err)
	}
	if !HookInstalled(repo, HookScopeProject) {
		t.Error("project scope not installed")
	}
	if HookInstalled(repo, HookScopeLocal) {
		t.Error("local scope must be untouched by a project install")
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.json")); err != nil {
		t.Errorf("settings.json missing: %v", err)
	}
}

func TestHookInstallCorruptSettings(t *testing.T) {
	repo := t.TempDir()
	path := settingsPath(repo, HookScopeLocal)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallHook(repo, HookScopeLocal); err == nil {
		t.Error("InstallHook on corrupt settings must error, not clobber")
	}
	if _, _, err := UninstallHook(repo, HookScopeLocal); err == nil {
		t.Error("UninstallHook on corrupt settings must error")
	}
	if HookInstalled(repo, HookScopeLocal) {
		t.Error("HookInstalled on corrupt settings must be false")
	}
	// The corrupt file was not overwritten.
	data, _ := os.ReadFile(path)
	if string(data) != "{not json" {
		t.Errorf("corrupt file was rewritten: %q", data)
	}
}

// An empty or whitespace-only settings file is treated as empty settings.
func TestHookInstallEmptySettingsFile(t *testing.T) {
	repo := t.TempDir()
	path := settingsPath(repo, HookScopeLocal)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := InstallHook(repo, HookScopeLocal); err != nil || !changed {
		t.Fatalf("install on empty file = (changed=%v, err=%v)", changed, err)
	}
	if !HookInstalled(repo, HookScopeLocal) {
		t.Error("HookInstalled false after installing into empty file")
	}
}

// A "hooks" key that is not an object is tolerated (replaced), not a
// crash — current behavior via asMap's fresh-map fallback.
func TestHookInstallNonObjectHooksKey(t *testing.T) {
	repo := t.TempDir()
	path := settingsPath(repo, HookScopeLocal)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, path, map[string]any{"hooks": "oops", "keep": true})
	if _, changed, err := InstallHook(repo, HookScopeLocal); err != nil || !changed {
		t.Fatalf("install = (changed=%v, err=%v)", changed, err)
	}
	root := readJSON(t, path)
	if _, ok := root["hooks"].(map[string]any); !ok {
		t.Errorf("hooks not replaced by an object: %+v", root["hooks"])
	}
	if root["keep"] != true {
		t.Error("unrelated key dropped")
	}
}
