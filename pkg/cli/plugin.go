package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// PluginInfo returns the full view for one plugin (incl. config schema +
// current values via ViewFor) plus its manifest.
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
	v, _ := reg.ViewFor(name)
	return v, &m, nil
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
// ~/.iterion/plugins/<name>/ and returns the installed plugin's name. The
// mechanism lives in pkg/plugin so the CLI and the HTTP server install
// identically; see plugin.Install.
func PluginInstall(ctx context.Context, src string) (string, error) {
	return plugin.Install(ctx, src)
}

// PluginUninstall removes an installed plugin (delegates to plugin.Uninstall).
func PluginUninstall(name string) error {
	return plugin.Uninstall(name)
}

// PluginConfigView returns a plugin's view (config schema + masked values).
func PluginConfigView(name string) (PluginView, error) {
	reg, err := plugin.Load()
	if err != nil {
		return PluginView{}, err
	}
	v, ok := reg.ViewFor(name)
	if !ok {
		return PluginView{}, fmt.Errorf("plugin %q not found", name)
	}
	return v, nil
}

// PluginConfigSet parses "key=value" --set flags and applies them to a plugin
// (accepting only declared fields, keeping a secret left blank), returning the
// refreshed view. Parsing shares the package's kv-flag scanner.
func PluginConfigSet(name string, sets []string) (PluginView, error) {
	values, err := parseKVPairs[string](sets, kvOpts[string]{
		errFmt:            "invalid --set %q (want key=value)",
		trimKey:           true,
		requireTrimmedKey: true,
	})
	if err != nil {
		return PluginView{}, err
	}
	reg, err := plugin.Load()
	if err != nil {
		return PluginView{}, err
	}
	if err := reg.ApplyConfig(name, values); err != nil {
		return PluginView{}, err
	}
	v, _ := reg.ViewFor(name)
	return v, nil
}
