package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// SuperviseOptions configures `iterion supervise` — the attach path that
// points an LLM supervisor at an already-running run (the in-.bot
// `supervisor <name>:` declaration auto-spawns the same coordinator and
// needs none of this). The supervisor watches the run's event stream
// and enqueues steering messages the run picks up at its next turn.
type SuperviseOptions struct {
	RunID    string
	Session  string // raw Claude Code session: cwd or session id (mutually exclusive with RunID)
	Name     string
	Model    string
	System   string   // inline policy text, or @path to read from a file
	Nodes    []string // node ids to watch (empty = whole run)
	Monitors []string // pre-declared monitors as "key=val,key=val" specs
	Cooldown time.Duration
	MaxEvals int
	StoreDir string
}

// RunSupervise attaches a supervisor to either an iterion run
// (--run-id) or a raw Claude Code session (--claude-session) and blocks
// until the target ends or the operator interrupts (SIGINT/SIGTERM).
func RunSupervise(p *Printer, opts SuperviseOptions) error {
	if (opts.RunID == "") == (opts.Session == "") {
		return fmt.Errorf("supervise: pass exactly one of --run-id or --claude-session")
	}
	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)

	system, err := resolveSystemPolicy(opts.System)
	if err != nil {
		return err
	}
	monitors, err := supervise.ParseMonitorSpecs(opts.Monitors)
	if err != nil {
		return fmt.Errorf("--monitor: %w", err)
	}
	name := opts.Name
	if name == "" {
		name = "supervisor"
	}

	if opts.Session != "" {
		return runSuperviseClaudeSession(p, opts, name, system, monitors, logger)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)
	svc, err := runview.NewService(storeDir)
	if err != nil {
		return fmt.Errorf("supervise: open store: %w", err)
	}

	spec := supervise.Spec{
		Name: name, Model: opts.Model, System: system,
		Watches: opts.Nodes, Monitors: monitors,
		Cooldown: opts.Cooldown, MaxEvals: opts.MaxEvals,
	}
	coord := supervise.New(svc, svc, opts.RunID, spec, nil, logger)
	if coord == nil {
		return fmt.Errorf("supervise: could not start coordinator (missing service or run id)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	coord.Start(ctx)

	scope := "whole run"
	if len(opts.Nodes) > 0 {
		scope = "nodes " + strings.Join(opts.Nodes, ", ")
	}
	p.Line("Supervising run %s (%s). Press Ctrl-C to detach.", opts.RunID, scope)
	select {
	case <-ctx.Done():
	case <-coord.Done():
	}
	coord.Close()
	p.Line("Supervisor detached from run %s.", opts.RunID)
	return nil
}

// runSuperviseClaudeSession attaches to a raw Claude Code CLI/VSCode
// session: it tails the session transcript and steers via the
// hook-drained inbox. A raw session has no nodes, so --node is ignored.
func runSuperviseClaudeSession(p *Printer, opts SuperviseOptions, name, system string, monitors []supervise.Monitor, logger *iterlog.Logger) error {
	cwd, _ := os.Getwd()
	sess, err := supervise.ResolveClaudeSession(opts.Session, cwd)
	if err != nil {
		return err
	}
	if sess.TranscriptPath == "" {
		return fmt.Errorf("supervise: no Claude Code transcript found for %q — start a `claude` session in that directory first", opts.Session)
	}
	if !supervise.HookInstalled(sess.Cwd, supervise.HookScopeLocal) && sess.Cwd != "" {
		p.Line("warning: the drain hook is not installed in %s/.claude/settings.local.json — run `iterion supervise install-hook` so injected messages are delivered.", sess.Cwd)
	}
	inj, err := supervise.NewInboxInjector(sess.ProjectKey, sess.SessionID)
	if err != nil {
		return err
	}
	obs := supervise.NewTranscriptObserver(sess.TranscriptPath)

	spec := supervise.Spec{
		Name: name, Model: opts.Model, System: system,
		Monitors: monitors, // Watches empty: raw sessions are always armed
		Cooldown: opts.Cooldown, MaxEvals: opts.MaxEvals,
	}
	coord := supervise.New(obs, inj, sess.SessionID, spec, nil, logger)
	if coord == nil {
		return fmt.Errorf("supervise: could not start coordinator")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	coord.Start(ctx)
	p.Line("Supervising Claude Code session %s (cwd %s). Press Ctrl-C to detach.", sess.SessionID, sess.Cwd)
	<-ctx.Done()
	coord.Close()
	p.Line("Supervisor detached.")
	return nil
}

// RunSuperviseInstallHook installs (or removes, when remove=true) the
// drain hook in the target repo's Claude Code settings.
func RunSuperviseInstallHook(p *Printer, repoDir string, project, remove bool) error {
	if repoDir == "" {
		repoDir, _ = os.Getwd()
	}
	scope := supervise.HookScopeLocal
	if project {
		scope = supervise.HookScopeProject
	}
	if remove {
		path, changed, err := supervise.UninstallHook(repoDir, scope)
		if err != nil {
			return err
		}
		if changed {
			p.Line("Removed the iterion drain hook from %s.", path)
		} else {
			p.Line("No iterion drain hook found in %s.", path)
		}
		return nil
	}
	path, changed, err := supervise.InstallHook(repoDir, scope)
	if err != nil {
		return err
	}
	if changed {
		p.Line("Installed the iterion drain hook in %s.", path)
	} else {
		p.Line("The iterion drain hook is already installed in %s.", path)
	}
	return nil
}

// resolveSystemPolicy reads the supervision policy: an @path prefix
// loads the text from a file; otherwise the value is the inline policy.
func resolveSystemPolicy(s string) (string, error) {
	if strings.HasPrefix(s, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(s, "@"))
		if err != nil {
			return "", fmt.Errorf("supervise: read policy file: %w", err)
		}
		return string(data), nil
	}
	return s, nil
}
