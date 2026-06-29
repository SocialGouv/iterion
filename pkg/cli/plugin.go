package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// PluginView is the listing-facing projection of a loaded plugin (an alias of
// plugin.View so the CLI and the HTTP server render plugins identically).
type PluginView = plugin.View

// PluginList loads the registry and returns every plugin as a view.
func PluginList() ([]PluginView, error) {
	reg, err := plugin.Load()
	if err != nil {
		return nil, err
	}
	return reg.Views(), nil
}

// PluginInfo returns the full view for one plugin.
func PluginInfo(name string) (PluginView, *plugin.Manifest, error) {
	reg, err := plugin.Load()
	if err != nil {
		return PluginView{}, nil, err
	}
	p, ok := reg.Get(name)
	if !ok {
		return PluginView{}, nil, fmt.Errorf("plugin %q not found", name)
	}
	m := p.Manifest
	return p.View(), &m, nil
}

// PluginSetEnabled enables or disables a plugin and persists the decision.
func PluginSetEnabled(name string, enabled bool) error {
	reg, err := plugin.Load()
	if err != nil {
		return err
	}
	return reg.SetEnabled(name, enabled)
}

// PluginRun executes a plugin's lifecycle command ("index" or "refresh") in the
// given workspace (default: cwd), streaming output to stdout/stderr.
func PluginRun(ctx context.Context, name, phase, workspace string) error {
	reg, err := plugin.Load()
	if err != nil {
		return err
	}
	p, ok := reg.Get(name)
	if !ok {
		return fmt.Errorf("plugin %q not found", name)
	}
	lc := p.Manifest.Contributes.Lifecycle
	if lc == nil {
		return fmt.Errorf("plugin %q has no lifecycle commands", name)
	}
	var cmdStr string
	switch phase {
	case "index":
		cmdStr = lc.Index
	case "refresh":
		cmdStr = lc.Refresh
	default:
		return fmt.Errorf("unknown lifecycle phase %q (want index|refresh)", phase)
	}
	if strings.TrimSpace(cmdStr) == "" {
		return fmt.Errorf("plugin %q has no %q command", name, phase)
	}
	if workspace == "" {
		if wd, werr := os.Getwd(); werr == nil {
			workspace = wd
		}
	}
	expanded := reg.ExpandContextFor(name, workspace).Expand(cmdStr)
	if cdErr := os.MkdirAll(reg.CacheDir(name), 0o755); cdErr != nil {
		return cdErr
	}
	c := exec.CommandContext(ctx, "sh", "-c", expanded)
	c.Dir = workspace
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// PluginInstall installs a plugin from a local directory or git URL into
// ~/.iterion/plugins/<name>/ and returns the installed plugin's name. A git URL
// is shallow-cloned; a local path's plugin.yaml is validated then copied.
func PluginInstall(ctx context.Context, src string) (string, error) {
	reg, err := plugin.Load()
	if err != nil {
		return "", err
	}
	srcDir := src
	if isGitURL(src) {
		tmp, terr := os.MkdirTemp("", "iterion-plugin-clone-*")
		if terr != nil {
			return "", terr
		}
		defer os.RemoveAll(tmp)
		clone := exec.CommandContext(ctx, "git", "clone", "--depth=1", src, tmp)
		clone.Stdout = os.Stderr // progress to stderr, never stdout (JSON-clean)
		clone.Stderr = os.Stderr
		if cerr := clone.Run(); cerr != nil {
			return "", fmt.Errorf("plugin: git clone %q: %w", src, cerr)
		}
		srcDir = tmp
	}
	data, rerr := os.ReadFile(filepath.Join(srcDir, plugin.ManifestFile))
	if rerr != nil {
		return "", fmt.Errorf("plugin: %s not found in %q: %w", plugin.ManifestFile, src, rerr)
	}
	m, perr := plugin.ParseManifest(data)
	if perr != nil {
		return "", perr
	}
	dest := reg.InstallDir(m.Name)
	if _, ok := reg.Get(m.Name); ok {
		// Overwrite an existing installed plugin (upgrade); builtins of the
		// same name still win at load and the install is shadowed — warn-free
		// here, surfaced by `plugin list`.
		if rmErr := os.RemoveAll(dest); rmErr != nil {
			return "", rmErr
		}
	}
	if err := copyTree(srcDir, dest); err != nil {
		return "", fmt.Errorf("plugin: install %q: %w", m.Name, err)
	}
	return m.Name, nil
}

// PluginUninstall removes an installed plugin.
func PluginUninstall(name string) error {
	reg, err := plugin.Load()
	if err != nil {
		return err
	}
	return reg.Remove(name)
}

func isGitURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git")
}

// copyTree recursively copies srcDir into dstDir (files + dirs), skipping a
// nested .git directory so a cloned plugin doesn't carry its repo metadata.
func copyTree(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	// Stable order for deterministic installs.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		s := filepath.Join(srcDir, e.Name())
		d := filepath.Join(dstDir, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
