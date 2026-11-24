package plugin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	hooks "github.com/SocialGouv/iterion/pkg/claw/internal/hooks"
)

func writeValidPluginSource(t *testing.T, dir, name, version string) string {
	t.Helper()
	src := filepath.Join(dir, name+"-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	manifest := `{
		"name": "` + name + `",
		"version": "` + version + `",
		"description": "fixture"
	}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return src
}

func newRunner() *hooks.Runner {
	return hooks.NewRunner(hooks.WithLogger(io.Discard))
}

func TestPrePluginInstallAbortsInstall(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "blocked", "0.1.0")
	configHome := filepath.Join(dir, "config")

	r := newRunner()
	var preFires, postFires int
	r.Register(hooks.PrePluginInstall, func(_ context.Context, hctx hooks.Context) (hooks.Decision, error) {
		preFires++
		if hctx.Plugin == nil {
			t.Errorf("PrePluginInstall: nil Plugin payload")
		} else if hctx.Plugin.Name != "blocked" {
			t.Errorf("PrePluginInstall: Plugin.Name = %q, want %q", hctx.Plugin.Name, "blocked")
		}
		return hooks.Decision{Action: hooks.ActionBlock, Reason: "policy"}, nil
	})
	r.Register(hooks.PostPluginInstall, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		postFires++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome}, WithHooks(r))
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	outcome, err := mgr.InstallContext(context.Background(), src)
	if err == nil {
		t.Fatal("InstallContext: expected error after Block, got nil")
	}
	if outcome != nil {
		t.Errorf("InstallContext: outcome = %+v, want nil", outcome)
	}
	if preFires != 1 {
		t.Errorf("PrePluginInstall fires = %d, want 1", preFires)
	}
	if postFires != 0 {
		t.Errorf("PostPluginInstall must NOT fire when Pre blocks, got %d", postFires)
	}

	if _, ok := mgr.registry.Plugins["blocked@external"]; ok {
		t.Error("blocked plugin must NOT be in registry")
	}
	installDir := filepath.Join(configHome, "plugins", "installed", "blocked-external")
	if _, statErr := os.Stat(installDir); !os.IsNotExist(statErr) {
		t.Errorf("install directory must NOT exist after Block, stat err = %v", statErr)
	}
}

func TestPostPluginInstallFires(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "ok-plugin", "1.2.3")
	configHome := filepath.Join(dir, "config")

	r := newRunner()
	var seen hooks.Context
	var postFires int
	r.Register(hooks.PostPluginInstall, func(_ context.Context, hctx hooks.Context) (hooks.Decision, error) {
		postFires++
		seen = hctx
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome}, WithHooks(r))
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	outcome, err := mgr.InstallContext(context.Background(), src)
	if err != nil {
		t.Fatalf("InstallContext: %v", err)
	}
	if outcome.PluginID != "ok-plugin@external" {
		t.Errorf("PluginID = %q", outcome.PluginID)
	}
	if postFires != 1 {
		t.Fatalf("PostPluginInstall fires = %d, want 1", postFires)
	}
	if seen.Plugin == nil {
		t.Fatal("PostPluginInstall: Plugin payload nil")
	}
	if seen.Plugin.Error != nil {
		t.Errorf("Plugin.Error = %v, want nil on success", seen.Plugin.Error)
	}
	if seen.Plugin.Name != "ok-plugin" || seen.Plugin.Version != "1.2.3" {
		t.Errorf("Plugin payload mismatch: %+v", seen.Plugin)
	}
	if seen.Plugin.InstallPath == "" {
		t.Error("Plugin.InstallPath should be populated")
	}
}

func TestPostPluginInstallFiresOnFailure(t *testing.T) {
	dir := t.TempDir()
	srcOK := writeValidPluginSource(t, dir, "fails-on-copy", "0.0.1")

	// Force copyDir to fail by pointing InstallRoot at an unwritable parent.
	installRoot := filepath.Join(dir, "ro-root")
	if err := os.MkdirAll(installRoot, 0o500); err != nil {
		t.Fatalf("mkdir ro-root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(installRoot, 0o755) })

	r := newRunner()
	var preFires, postFires int
	r.Register(hooks.PrePluginInstall, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		preFires++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.PostPluginInstall, func(_ context.Context, hctx hooks.Context) (hooks.Decision, error) {
		postFires++
		if hctx.Plugin == nil || hctx.Plugin.Error == nil {
			t.Errorf("PostPluginInstall on failure: expected non-nil Error, got Plugin=%+v", hctx.Plugin)
		}
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	mgr, err := NewPluginManager(PluginManagerConfig{
		ConfigHome:  filepath.Join(dir, "config"),
		InstallRoot: installRoot,
	}, WithHooks(r))
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	_, installErr := mgr.InstallContext(context.Background(), srcOK)
	if installErr == nil {
		t.Skip("install unexpectedly succeeded (likely running as root) — skipping failure path assertion")
	}
	if preFires != 1 {
		t.Errorf("PrePluginInstall fires = %d, want 1", preFires)
	}
	if postFires != 1 {
		t.Errorf("PostPluginInstall fires = %d, want 1 (must fire on failure)", postFires)
	}
	if _, ok := mgr.registry.Plugins["fails-on-copy@external"]; ok {
		t.Error("failed install must not appear in registry")
	}
}

func TestPrePluginUninstallAbortsUninstall(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "keep-me", "0.1.0")
	configHome := filepath.Join(dir, "config")

	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if _, err := mgr.Install(src); err != nil {
		t.Fatalf("Install: %v", err)
	}

	r := newRunner()
	var preFires, postFires int
	r.Register(hooks.PrePluginUninstall, func(_ context.Context, hctx hooks.Context) (hooks.Decision, error) {
		preFires++
		if hctx.Plugin == nil || hctx.Plugin.Name != "keep-me" {
			t.Errorf("PrePluginUninstall: Plugin payload mismatch: %+v", hctx.Plugin)
		}
		return hooks.Decision{Action: hooks.ActionBlock, Reason: "still in use"}, nil
	})
	r.Register(hooks.PostPluginUninstall, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		postFires++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	mgr.hooks = r

	id := "keep-me@external"
	err = mgr.UninstallContext(context.Background(), id)
	if err == nil {
		t.Fatal("UninstallContext: expected error after Block, got nil")
	}
	if preFires != 1 {
		t.Errorf("Pre fires = %d, want 1", preFires)
	}
	if postFires != 0 {
		t.Errorf("Post must not fire when Pre blocks, got %d", postFires)
	}
	if _, ok := mgr.registry.Plugins[id]; !ok {
		t.Error("plugin must remain in registry after blocked uninstall")
	}
	installDir := filepath.Join(configHome, "plugins", "installed", "keep-me-external")
	if _, statErr := os.Stat(installDir); statErr != nil {
		t.Errorf("install dir must remain after blocked uninstall, stat err = %v", statErr)
	}
}

func TestPostPluginUninstallFires(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "uninstall-me", "0.1.0")
	configHome := filepath.Join(dir, "config")

	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if _, err := mgr.Install(src); err != nil {
		t.Fatalf("Install: %v", err)
	}

	r := newRunner()
	var seen hooks.Context
	var postFires int
	r.Register(hooks.PostPluginUninstall, func(_ context.Context, hctx hooks.Context) (hooks.Decision, error) {
		postFires++
		seen = hctx
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	mgr.hooks = r

	id := "uninstall-me@external"
	if err := mgr.UninstallContext(context.Background(), id); err != nil {
		t.Fatalf("UninstallContext: %v", err)
	}
	if postFires != 1 {
		t.Fatalf("Post fires = %d, want 1", postFires)
	}
	if seen.Plugin == nil {
		t.Fatal("Plugin payload nil")
	}
	if seen.Plugin.Error != nil {
		t.Errorf("Plugin.Error = %v, want nil on success", seen.Plugin.Error)
	}
	if _, ok := mgr.registry.Plugins[id]; ok {
		t.Error("plugin must be removed from registry on successful uninstall")
	}
}

func TestPostPluginUninstallFiresOnFailure(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "fails-removal", "0.1.0")
	configHome := filepath.Join(dir, "config")

	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if _, err := mgr.Install(src); err != nil {
		t.Fatalf("Install: %v", err)
	}

	id := "fails-removal@external"
	record := mgr.registry.Plugins[id]

	// Sabotage the install path so os.RemoveAll fails: replace it with a
	// non-empty read-only parent. We do this by chmod'ing the parent of
	// the install dir to read-only; on the test cleanup we restore perms.
	parent := filepath.Dir(record.InstallPath)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	r := newRunner()
	var postFires int
	r.Register(hooks.PostPluginUninstall, func(_ context.Context, hctx hooks.Context) (hooks.Decision, error) {
		postFires++
		if hctx.Plugin == nil {
			t.Errorf("Post: Plugin payload nil")
		} else if hctx.Plugin.Error == nil {
			t.Errorf("Post: expected Error to be set on failure")
		}
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	mgr.hooks = r

	uninstallErr := mgr.UninstallContext(context.Background(), id)
	if uninstallErr == nil {
		t.Skip("uninstall unexpectedly succeeded (likely running as root) — skipping failure assertion")
	}
	if postFires != 1 {
		t.Errorf("Post fires = %d, want 1 on failure path", postFires)
	}
}

func TestNilRunnerIsNoOp(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "no-hooks", "0.1.0")
	configHome := filepath.Join(dir, "config")

	mgr, err := NewPluginManager(PluginManagerConfig{ConfigHome: configHome})
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if mgr.hooks != nil {
		t.Fatalf("default hooks should be nil, got %v", mgr.hooks)
	}

	outcome, err := mgr.InstallContext(context.Background(), src)
	if err != nil {
		t.Fatalf("Install with nil Runner: %v", err)
	}
	if outcome == nil {
		t.Fatal("outcome nil")
	}
	if err := mgr.UninstallContext(context.Background(), outcome.PluginID); err != nil {
		t.Fatalf("Uninstall with nil Runner: %v", err)
	}
}

func TestWithHooksExplicitNilSafe(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "nil-runner", "0.1.0")

	mgr, err := NewPluginManager(
		PluginManagerConfig{ConfigHome: filepath.Join(dir, "config")},
		WithHooks(nil),
	)
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if _, err := mgr.InstallContext(context.Background(), src); err != nil {
		t.Fatalf("Install with WithHooks(nil): %v", err)
	}
}

func TestLegacyInstallUsesBackgroundContext(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "legacy", "0.1.0")

	r := newRunner()
	var pre int
	r.Register(hooks.PrePluginInstall, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		pre++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	mgr, err := NewPluginManager(
		PluginManagerConfig{ConfigHome: filepath.Join(dir, "config")},
		WithHooks(r),
	)
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}

	if _, err := mgr.Install(src); err != nil {
		t.Fatalf("Install (no ctx): %v", err)
	}
	if pre != 1 {
		t.Errorf("Pre fires through legacy Install = %d, want 1", pre)
	}
}

// Sanity: handler returning an error is logged + treated as Continue, so
// install proceeds.
func TestHandlerErrorTreatedAsContinue(t *testing.T) {
	dir := t.TempDir()
	src := writeValidPluginSource(t, dir, "err-handler", "0.1.0")

	r := newRunner()
	r.Register(hooks.PrePluginInstall, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		return hooks.Decision{}, errors.New("boom")
	})
	mgr, err := NewPluginManager(
		PluginManagerConfig{ConfigHome: filepath.Join(dir, "config")},
		WithHooks(r),
	)
	if err != nil {
		t.Fatalf("NewPluginManager: %v", err)
	}
	if _, err := mgr.InstallContext(context.Background(), src); err != nil {
		t.Fatalf("Install with erroring handler should still proceed: %v", err)
	}
}
