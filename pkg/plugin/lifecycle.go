package plugin

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// RunLifecycle executes a plugin's lifecycle command ("index" or "refresh") in
// the given workspace (default: cwd), streaming subprocess output to
// stdout/stderr. Placeholders ({{workspace}}, {{plugin.dir}}, {{plugin.cache}},
// {{config.<key>}}) are expanded before the command runs via `sh -c`; the
// plugin's cache directory is created so {{plugin.cache}} always resolves to an
// existing path. Shared by the CLI (`iterion plugin run`) and the HTTP server
// so both surfaces run lifecycles identically.
func RunLifecycle(ctx context.Context, reg *Registry, name, phase, workspace string, stdout, stderr io.Writer) error {
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
	c.Stdout = stdout
	c.Stderr = stderr
	return c.Run()
}
