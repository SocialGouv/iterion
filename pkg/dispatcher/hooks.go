package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Hook is a single lifecycle hook. Exactly one of Script or Path must be
// set: Script is an inline shell snippet (passed to `sh -lc`); Path is
// a path to a script (the path itself is the command — the file's
// shebang controls the interpreter).
type Hook struct {
	Script    string `yaml:"script,omitempty" json:"script,omitempty"`
	Path      string `yaml:"path,omitempty" json:"path,omitempty"`
	TimeoutMS int    `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
}

// Hooks groups the four workspace-lifecycle hooks. Any field can be nil.
type Hooks struct {
	AfterCreate  *Hook `yaml:"after_create,omitempty" json:"after_create,omitempty"`
	BeforeRun    *Hook `yaml:"before_run,omitempty" json:"before_run,omitempty"`
	AfterRun     *Hook `yaml:"after_run,omitempty" json:"after_run,omitempty"`
	BeforeRemove *Hook `yaml:"before_remove,omitempty" json:"before_remove,omitempty"`
}

// defaultHookTimeout caps any hook missing an explicit timeout_ms.
const defaultHookTimeout = 60 * time.Second

// Validate checks that each hook in the group sets exactly one of
// Script or Path.
func (h *Hooks) Validate() error {
	cases := []struct {
		name string
		hook *Hook
	}{
		{"after_create", h.AfterCreate},
		{"before_run", h.BeforeRun},
		{"after_run", h.AfterRun},
		{"before_remove", h.BeforeRemove},
	}
	for _, c := range cases {
		if c.hook == nil {
			continue
		}
		if (c.hook.Script == "") == (c.hook.Path == "") {
			return fmt.Errorf("hook %q: exactly one of script/path required", c.name)
		}
	}
	return nil
}

// Run executes the hook with cwd set to workspace and the additional
// env vars appended to the inherited environment. stdout/stderr lines
// are streamed to the logger. A nil hook is a no-op.
func (h *Hook) Run(ctx context.Context, logger *iterlog.Logger, name, workspace string, env []string) error {
	if h == nil {
		return nil
	}
	timeout := time.Duration(h.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := h.Script
	if command == "" {
		command = h.Path
	}
	if command == "" {
		return fmt.Errorf("hook %s: empty command", name)
	}

	cmd := exec.CommandContext(cctx, "sh", "-lc", command)
	// Cancellation takes the whole group: a hook's backgrounded grandchild
	// would otherwise outlive the timeout, keep writing the workspace the
	// next lifecycle step is about to act on, and hold the inherited
	// stdout/stderr fds.
	proc.TerminateGroupOnCancel(cmd)
	// WaitDelay bounds the orphan-pipe wait for anything that escaped the
	// group (a descendant that made its own session). Without it, cmd.Run
	// blocks until that process exits naturally, defeating the timeout. 2s
	// is well below any sane TimeoutMS user setting.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = workspace
	// Inherit the host environment so hooks can find `git`, `gh`, …
	// on $PATH. The dispatcher-supplied env (ITERION_*) is appended so
	// it overrides any conflicting parent value.
	cmd.Env = append(os.Environ(), env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	logger.Info("dispatcher: hook %s starting (cwd=%s timeout=%s)", name, workspace, timeout)
	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	if out := strings.TrimSpace(stdout.String()); out != "" {
		for _, line := range strings.Split(out, "\n") {
			logger.Info("dispatcher: hook %s: %s", name, line)
		}
	}
	if errOut := strings.TrimSpace(stderr.String()); errOut != "" {
		for _, line := range strings.Split(errOut, "\n") {
			logger.Warn("dispatcher: hook %s [stderr]: %s", name, line)
		}
	}

	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("hook %s: timeout after %s", name, timeout)
		}
		return fmt.Errorf("hook %s: %w (after %s)", name, err, dur)
	}
	logger.Info("dispatcher: hook %s ok (%s)", name, dur)
	return nil
}
